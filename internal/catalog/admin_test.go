package catalog_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

var superActor = audit.Actor{AdminID: 1, IP: "10.0.0.1"}

func TestSetPrice(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	for _, c := range []struct {
		d      catalog.Duration
		amount int64
	}{{"1h", 100}, {catalog.D1d, 0}, {catalog.D1d, catalog.MaxPrice + 1}} {
		if _, err := svc.SetPrice(ctx, superActor, pid, c.d, c.amount, true); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("SetPrice(%s, %d) = %v", c.d, c.amount, err)
		}
	}
	if _, err := svc.SetPrice(ctx, superActor, 9999, catalog.D1d, 100, true); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown product = %v", err)
	}
	p, err := svc.SetPrice(ctx, superActor, pid, catalog.D1d, 7900, true)
	if err != nil || p.Amount != 7900 || !p.Enabled {
		t.Fatalf("set = %+v, %v", p, err)
	}
	if amount, _ := svc.Quote(ctx, pool, pid, catalog.D1d); amount != 7900 {
		t.Fatalf("quote = %d", amount)
	}
	if _, err := svc.SetPrice(ctx, superActor, pid, catalog.D1w, 41400, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Quote(ctx, pool, pid, catalog.D1w); err == nil {
		t.Fatal("disabled price must not be quotable")
	}
	products, err := svc.AdminProducts(ctx)
	if err != nil || len(products) != 1 || len(products[0].Prices) != 3 || products[0].Prices[1].Enabled {
		t.Fatalf("admin products = %+v, %v", products, err)
	}
	var diff string
	_ = pool.QueryRow(ctx, `SELECT diff::text FROM audit_logs WHERE action = 'price.update' ORDER BY id LIMIT 1`).Scan(&diff)
	if diff == "" || !strings.Contains(diff, `"before"`) || !strings.Contains(diff, "6900") || !strings.Contains(diff, "7900") {
		t.Fatalf("price audit diff = %s", diff)
	}
}

func TestImagesAdmin(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	for _, in := range []catalog.ImageInput{
		{Name: "", Ref: "registry/x:1"}, {Name: "x", Ref: ""}, {Name: "x", Ref: "has space:1"},
		{Name: "x", Ref: "--privileged"}, {Name: "x", Ref: "-v/:/host"}, {Name: "x", Ref: "Registry/x:1"}, {Name: "x", Ref: "/abs:1"},
	} {
		if _, err := svc.CreateImage(ctx, superActor, in); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("CreateImage(%+v) = %v", in, err)
		}
	}
	img, err := svc.CreateImage(ctx, superActor, catalog.ImageInput{Name: "SGLang", Ref: "registry.spark.internal/base/sglang:0.5", Enabled: true, Sort: 40})
	if err != nil || img.ID == 0 || img.Ref != "registry.spark.internal/base/sglang:0.5" {
		t.Fatalf("create = %+v, %v", img, err)
	}
	if _, err := svc.UpdateImage(ctx, superActor, img.ID, catalog.ImageInput{Name: "SGLang", Ref: img.Ref, Enabled: false, Sort: 40}); err != nil {
		t.Fatal(err)
	}
	public, _ := svc.ListImages(ctx)
	for _, p := range public {
		if p.ID == img.ID {
			t.Fatal("disabled image must be hidden from users")
		}
	}
	all, err := svc.AdminImages(ctx)
	if err != nil || len(all) != 4 {
		t.Fatalf("admin images = %d, %v", len(all), err)
	}
	if _, err := svc.UpdateImage(ctx, superActor, 9999, catalog.ImageInput{Name: "x", Ref: "y"}); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
}
