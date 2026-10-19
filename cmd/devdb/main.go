// devdb 启动一个持久化的本地 PostgreSQL，供本地开发使用（端口 5433，数据在 .devdata/）。Ctrl+C 停止。
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

func main() {
	dir, err := filepath.Abs(".devdata")
	if err != nil {
		log.Fatal(err)
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(5433).
		Database("spark").
		DataPath(filepath.Join(dir, "pg")).
		RuntimePath(filepath.Join(dir, "runtime")))
	if err := pg.Start(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("PostgreSQL 已启动：postgres://postgres:postgres@localhost:5433/spark?sslmode=disable")
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	if err := pg.Stop(); err != nil {
		log.Fatal(err)
	}
}
