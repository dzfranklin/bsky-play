package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	ievents "github.com/bluesky-social/indigo/events"
	isequential "github.com/bluesky-social/indigo/events/schedulers/sequential"
	irepo "github.com/bluesky-social/indigo/repo"
	"github.com/gorilla/websocket"
	typegen "github.com/whyrusleeping/cbor-gen"
	"log"
	"log/slog"
	"modernc.org/sqlite"
	_ "modernc.org/sqlite"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

/* References:

- https://docs.bsky.app/docs/api/

- https://public.api.bsky.app/xrpc/app.bsky.actor.getProfile?actor=stamos.org

*/

type backuper interface {
	NewBackup(string) (*sqlite.Backup, error)
	NewRestore(string) (*sqlite.Backup, error)
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	dataDir := flag.String("data", "/data", "Data dir")
	flag.Parse()
	if *dataDir == "" {
		panic("-data is required")
	}

	queryDBPath := *dataDir + "/query.db?_pragma=journal_mode%3DWAL"

	db, err := sql.Open("sqlite", *dataDir+"/data.db?_pragma=journal_mode%3DWAL")
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS posts (
    	created_at timestamp,
    	repo_did text,
    	url text,
    	reply_parent_uri text,
    	reply_root_uri text,
    	json jsonb
	)`)
	if err != nil {
		log.Fatal("Create posts table: ", err)
	}

	con, _, err := websocket.DefaultDialer.Dial("wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos", http.Header{})
	if err != nil {
		panic(err)
	}

	rsc := &ievents.RepoStreamCallbacks{
		RepoCommit: func(evt *atproto.SyncSubscribeRepos_Commit) error {
			repo, err := irepo.ReadRepoFromCar(ctx, bytes.NewReader(evt.Blocks))
			if err != nil {
				slog.Debug(err.Error())
				return nil
			}

			for _, op := range evt.Ops {
				err := handleOp(ctx, db, repo, op)
				if err != nil {
					slog.Debug(err.Error())
				}
			}

			return nil
		},
	}

	var quitWg sync.WaitGroup
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("Quitting")
		cancel()
	}()

	quitWg.Add(1)
	go func() {
		defer quitWg.Done()
		for {
			slog.Info("Deleting old posts")
			_, err := db.Exec(`DELETE FROM posts WHERE created_at <= date('now', '-7 day')`)
			if err != nil {
				slog.Error("delete old posts: " + err.Error())
			}

			timer := time.NewTimer(time.Hour)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()

	quitWg.Add(1)
	go func() {
		defer quitWg.Done()
		for {
			srcConn, err := db.Conn(ctx)
			if err != nil {
				log.Fatal(err)
			}
			err = srcConn.Raw(func(srcConn any) error {
				bck, err := srcConn.(backuper).NewBackup(queryDBPath)
				if err != nil {
					return err
				}
				for more := true; more; {
					more, err = bck.Step(-1)
					if err != nil {
						return err
					}
				}
				return bck.Finish()
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Fatal("copy to query database: " + err.Error())
			}

			timer := time.NewTimer(time.Minute)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()

	sched := isequential.NewScheduler("myfirehose", rsc.EventHandler)
	err = ievents.HandleRepoStream(ctx, con, sched)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("ievents.HandleRepoStream: " + err.Error())
	}
	slog.Info("Done handling repo stream")

	err = db.Close()
	if err != nil {
		log.Fatal("store.Close: " + err.Error())
	}
	slog.Info("Closed database")

	slog.Info("Waiting for quit to complete")
	quitWg.Wait()
	slog.Info("Done")
}

func handleOp(ctx context.Context, db *sql.DB, repo *irepo.Repo, op *atproto.SyncSubscribeRepos_RepoOp) error {
	_, recCBOR, err := repo.GetRecord(ctx, op.Path)
	if err != nil {
		return err
	}

	rec, ok := recCBOR.(typegen.CBORMarshaler)
	if !ok {
		return err
	}

	switch rec := rec.(type) {
	case *bsky.FeedPost:
		postURL := fmt.Sprintf("https://bsky.app/profile/%s/post/%s\n", repo.RepoDid(), strings.TrimPrefix(op.Path, "app.bsky.feed.post/"))

		createdAt, err := time.Parse(time.RFC3339, rec.CreatedAt)
		if err != nil {
			return nil
		}

		recJSON, err := json.Marshal(rec)
		if err != nil {
			return err
		}

		var replyParent, replyRoot *string
		if rec.Reply != nil {
			replyRoot = &rec.Reply.Root.Uri
			replyParent = &rec.Reply.Parent.Uri
		}

		_, err = db.Exec(`INSERT INTO posts
    		(created_at, repo_did, url, reply_parent_uri, reply_root_uri, json)
			VALUES (?, ?, ?, ?, ?, ?)`,
			createdAt, repo.RepoDid(), postURL, replyParent, replyRoot, recJSON)
		if err != nil {
			log.Fatal(err)
		}
	}

	return nil
}
