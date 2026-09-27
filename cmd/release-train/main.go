// Command release-train 启动发布列车 HTTP 服务。
//
// 用法：
//
//	release-train -addr :8080 -state ./data/state.json
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	releasetrain "github.com/chris64233/go-release-train"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	statePath := flag.String("state", "data/state.json", "状态持久化文件路径（空字符串表示纯内存）")
	flag.Parse()

	store, err := releasetrain.NewFileStore(*statePath)
	if err != nil {
		log.Fatalf("open state store %q: %v", *statePath, err)
	}
	svc := releasetrain.NewService(store)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           releasetrain.NewHandler(svc),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("release-train listening on %s (state: %q)", *addr, *statePath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}
