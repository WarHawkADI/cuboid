// Command cuboid runs the multi-tenant aggregation layer: it reads the main
// database's change stream, keeps exact rollups in the aggregate store and
// serves the dashboard API, the monitoring page and the operator controls.
//
//	cuboid           serve (default)
//	cuboid keys      print the demo keys (demo mode) or create a random key
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"cuboid/internal/auth"
	"cuboid/internal/core"
	"cuboid/internal/store"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func options(log *slog.Logger) core.Options {
	interval, err := time.ParseDuration(env("CUBOID_FLUSH_INTERVAL", "1s"))
	if err != nil {
		interval = time.Second
	}
	demo, _ := strconv.ParseBool(env("CUBOID_DEMO_MODE", "false"))
	return core.Options{
		MainCDCDSN:    env("CUBOID_MAIN_CDC_DSN", "postgres://cuboid_cdc:cuboid_cdc@localhost:5433/shop"),
		MainVerifyDSN: env("CUBOID_MAIN_VERIFY_DSN", "postgres://cuboid_verify:cuboid_verify@localhost:5433/shop"),
		AggWriterDSN:  env("CUBOID_AGG_WRITER_DSN", "postgres://cuboid_writer:cuboid_writer@localhost:5434/agg"),
		AggReaderDSN:  env("CUBOID_AGG_READER_DSN", "postgres://cuboid_reader:cuboid_reader@localhost:5434/agg"),
		MetricsPath:   env("CUBOID_METRICS", "config/metrics.yaml"),
		LimitsPath:    env("CUBOID_LIMITS", "config/limits.yaml"),
		Pepper:        env("CUBOID_KEY_PEPPER", "dev-pepper-change-me"),
		DemoMode:      demo,
		DemoSeed:      env("CUBOID_DEMO_KEY_SEED", "cuboid-demo"),
		FlushInterval: interval,
		Publication:   env("CUBOID_PUBLICATION", "cuboid_pub"),
		StoreAppURL:   env("CUBOID_STOREAPP_URL", "http://localhost:8081"),
		LoadgenURL:    env("CUBOID_LOADGEN_URL", "http://localhost:8082"),
		NaiveURL:      env("CUBOID_NAIVE_URL", "http://localhost:8083"),
		ArtifactsDir:  env("CUBOID_ARTIFACTS", "artifacts"),
		WebDir:        env("CUBOID_WEB_DIR", "web/dist"),
		Log:           log,
	}
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, log)
	case "keys":
		err = keys(ctx, log)
	default:
		err = fmt.Errorf("unknown command %q (use serve or keys)", cmd)
	}
	if err != nil {
		log.Error("cuboid", "err", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, log *slog.Logger) error {
	opt := options(log)
	c, err := core.New(ctx, opt)
	if err != nil {
		return err
	}
	addr := env("CUBOID_LISTEN", ":8080")
	srv := &http.Server{Addr: addr, Handler: c.Routes(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		log.Info("serving", "addr", addr, "monitor", "http://localhost"+addr+"/monitor")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
		}
	}()
	go c.WatchConfig(ctx)
	runErr := c.Run(ctx)
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shut)
	return runErr
}

// keys prints the demo keys, or creates and prints a random key for a scope:
// cuboid keys store 42 | company 7 | operator.
func keys(ctx context.Context, log *slog.Logger) error {
	opt := options(log)
	st, err := store.Open(ctx, opt.AggWriterDSN, opt.AggReaderDSN, 2)
	if err != nil {
		return err
	}
	defer st.Close()
	k := auth.NewKeys(opt.Pepper)
	if len(os.Args) >= 3 {
		kind := auth.Kind(os.Args[2])
		var id int64
		if kind != auth.Operator {
			if len(os.Args) < 4 {
				return fmt.Errorf("usage: cuboid keys store <id> | company <id> | operator")
			}
			if id, err = strconv.ParseInt(os.Args[3], 10, 32); err != nil {
				return err
			}
		}
		tok := auth.Token(kind, int32(id), auth.NewSecret())
		if err := k.Save(ctx, st.W, tok, kind, int32(id)); err != nil {
			return err
		}
		fmt.Println(tok)
		return nil
	}
	if !opt.DemoMode {
		return fmt.Errorf("demo keys exist only with CUBOID_DEMO_MODE=true; create a key with: cuboid keys store <id>")
	}
	m := auth.NewMembership()
	if err := m.Load(ctx, st.W, "m_tenants", "tenant_id", "company_id", "name"); err != nil {
		return fmt.Errorf("%w (has Cuboid finished its first load?)", err)
	}
	demo := func(kind auth.Kind, id int32) string {
		return auth.Token(kind, id, auth.DemoSecret(opt.DemoSeed, kind, id))
	}
	fmt.Println("Demo keys (reproducible from CUBOID_DEMO_KEY_SEED; production keys are random):")
	fmt.Println()
	fmt.Printf("  operator        %s\n", demo(auth.Operator, 0))
	for _, t := range []int32{42, 43} {
		if _, ok := m.CompanyOf(t); ok {
			fmt.Printf("  store %-9d %s\n", t, demo(auth.Store, t))
		}
	}
	if co, ok := m.CompanyOf(42); ok {
		fmt.Printf("  company %-7d %s   (stores %v)\n", co, demo(auth.Company, co), m.Stores(co))
	}
	fmt.Println()
	fmt.Println("Every store and company has a key; the monitoring page's key picker lists them all.")
	return nil
}
