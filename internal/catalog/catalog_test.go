package catalog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func TestDurationLength(t *testing.T) {
	cases := map[catalog.Duration]time.Duration{
		catalog.D1d: 24 * time.Hour, catalog.D1w: 7 * 24 * time.Hour, catalog.D1m: 30 * 24 * time.Hour,
	}
	for d, want := range cases {
		if !d.Valid() || d.Length() != want {
			t.Fatalf("%s: valid=%v length=%v", d, d.Valid(), d.Length())
		}
	}
	for _, bad := range []catalog.Duration{"1h", "2d", ""} {
		if bad.Valid() {
			t.Fatalf("%q must be invalid", bad)
		}
	}
}

func TestListProductsWithSeededPrices(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	products, err := svc.ListProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(products) != 1 || products[0].Code != "spark-1" {
		t.Fatalf("products = %+v", products)
	}
	got := products[0].Prices
	want := []catalog.Price{{Duration: "1d", Amount: 6900}, {Duration: "1w", Amount: 41400}, {Duration: "1m", Amount: 159900}}
	if len(got) != len(want) {
		t.Fatalf("prices = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prices[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if products[0].Spec["memory_gb"].(float64) != 128 {
		t.Fatalf("spec = %+v", products[0].Spec)
	}
}

func TestQuote(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)

	amount, err := svc.Quote(ctx, pool, pid, catalog.D1w)
	if err != nil || amount != 41400 {
		t.Fatalf("Quote = %d, %v", amount, err)
	}
	if _, err := svc.Quote(ctx, pool, pid, "1h"); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("invalid duration want bad_request, got %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE prices SET enabled = false WHERE duration = '1d'`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Quote(ctx, pool, pid, catalog.D1d); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("disabled price want bad_request, got %v", err)
	}
}

func TestImages(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	images, err := svc.ListImages(ctx)
	if err != nil || len(images) != 3 || images[0].Name != "Ubuntu 24.04 + CUDA" {
		t.Fatalf("images = %+v, %v", images, err)
	}
	img, err := svc.GetImage(ctx, pool, images[1].ID)
	if err != nil || img.Ref != "registry.spark.internal/base/pytorch:latest" {
		t.Fatalf("GetImage = %+v, %v", img, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE images SET enabled = false WHERE id = $1`, img.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetImage(ctx, pool, img.ID); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("disabled image want bad_request, got %v", err)
	}
	if _, err := svc.GetImage(ctx, pool, 99999); err == nil {
		t.Fatal("missing image must error")
	}
}

func TestImageRefIgnoresEnabledFlag(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	id := dbtest.ImageID(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE images SET enabled = false WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	ref, err := svc.ImageRef(ctx, pool, id)
	if err != nil || ref == "" {
		t.Fatalf("ref = %q, %v", ref, err)
	}
	if _, err := svc.ImageRef(ctx, pool, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing image = %v", err)
	}
}
