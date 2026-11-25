// Command offerhound aggregates personalized credit-card offers from
// issuer portals you must be logged into — driven by declarative YAML.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/rajkumaar23/offerhound/internal/config"
	"github.com/rajkumaar23/offerhound/internal/engine"
	"github.com/rajkumaar23/offerhound/internal/store"
	"github.com/rajkumaar23/offerhound/internal/web"
	"github.com/spf13/cobra"
)

var (
	flagConfigs string
	flagDB      string
	flagServer  string
	flagToken   string
)

func main() {
	root := &cobra.Command{
		Use:   "offerhound",
		Short: "Your cards sniff out their own offers.",
	}
	home, _ := os.UserHomeDir()
	defDir := filepath.Join(home, ".offerhound")
	root.PersistentFlags().StringVar(&flagConfigs, "configs", filepath.Join(defDir, "configs"),
		"directory of bank YAML configs")
	root.PersistentFlags().StringVar(&flagDB, "db", filepath.Join(defDir, "offerhound.db"),
		"SQLite database path (local mode)")
	root.PersistentFlags().StringVar(&flagServer, "server", "",
		"serve URL to push results to (split mode; omit for local)")
	root.PersistentFlags().StringVar(&flagToken, "token", os.Getenv("OFFERHOUND_TOKEN"),
		"API token for --server (or OFFERHOUND_TOKEN)")

	root.AddCommand(cmdInit(), cmdVerify(), cmdSync(), cmdWorker(), cmdServe(), cmdSearch())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdInit() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the data directory and a serve token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := os.MkdirAll(flagConfigs, 0o755); err != nil {
				return err
			}
			token, err := store.NewToken()
			if err != nil {
				return err
			}
			tokenPath := filepath.Join(filepath.Dir(flagDB), "token")
			if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
				if err := os.MkdirAll(filepath.Dir(flagDB), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
					return err
				}
			}
			fmt.Printf("data dir:  %s\nconfigs:   %s\ntoken:     %s (also used as --token for workers)\n\n"+
				"Next: drop bank YAML files into the configs dir (see https://github.com/rajkumaar23/offerhound-configs)\n",
				filepath.Dir(flagDB), flagConfigs, token)
			return nil
		},
	}
}

func cmdVerify() *cobra.Command {
	return &cobra.Command{
		Use:   "verify [file.yaml|dir ...]",
		Short: "Validate bank configs (CI gate for the configs repo)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fail := false
			for _, a := range args {
				info, err := os.Stat(a)
				if err != nil {
					fmt.Printf("FAIL %s: %v\n", a, err)
					fail = true
					continue
				}
				var banks map[string]*config.Bank
				if info.IsDir() {
					banks, err = config.LoadDir(a)
				} else {
					b, e := config.Load(a)
					banks = map[string]*config.Bank{}
					if b != nil {
						banks[b.ID] = b
					}
					err = e
				}
				if err != nil {
					fmt.Printf("FAIL %s: %v\n", a, err)
					fail = true
					continue
				}
				ids := make([]string, 0, len(banks))
				for id := range banks {
					ids = append(ids, id)
				}
				sort.Strings(ids)
				for _, id := range ids {
					fmt.Printf("ok   %s (%d offer steps)\n", id, len(banks[id].Offers))
				}
			}
			if fail {
				return fmt.Errorf("config validation failed")
			}
			return nil
		},
	}
}

func loadBanks() (map[string]*config.Bank, error) {
	banks, err := config.LoadDir(flagConfigs)
	if err != nil {
		return nil, err
	}
	if len(banks) == 0 {
		return nil, fmt.Errorf("no bank configs found in %s", flagConfigs)
	}
	return banks, nil
}

// syncBank runs one bank and stores/ingests results.
func syncBank(ctx context.Context, b *config.Bank, worker string, verbose bool) error {
	st, err := store.Open(flagDB)
	if err != nil {
		return err
	}
	defer st.Close()

	br, err := engine.NewBrowser(b.Browser, b.BaseURL)
	if err != nil {
		return err
	}
	defer br.Close()

	runID, err := st.StartRun(b.ID, worker)
	if err != nil {
		return err
	}
	r := &engine.Runner{Bank: b, Browser: br, Log: os.Stderr,
		Gate: &engine.StoreGate{Store: st, RunID: runID}}
	res := r.Run(ctx)

	status, errMsg := "ok", ""
	if res.Err != nil {
		status, errMsg = "error", res.Err.Error()
	} else if !res.Complete {
		status, errMsg = "partial", strings.Join(res.SkippedLog, "; ")
	}
	st.FinishRun(runID, status, errMsg, len(res.Offers))
	if res.Err != nil {
		st.SetBankStatus(b.ID, "error", errMsg)
		return res.Err
	}
	note := fmt.Sprintf("collected %d offers", len(res.Offers))
	if err := st.Ingest(b.ID, res.Cards, res.Offers, res.Complete, note); err != nil {
		if err == store.ErrPartialRejected {
			fmt.Fprintf(os.Stderr, "[%s] partial collection rejected; last-good data kept\n", b.ID)
			return nil
		}
		return err
	}
	if flagServer != "" {
		c := engine.NewClient(flagServer, flagToken)
		if err := c.Ingest(ctx, engine.IngestReq{
			Bank: b.ID, Worker: worker, Complete: res.Complete, Note: note,
			Cards: res.Cards, Offers: res.Offers,
		}); err != nil {
			return fmt.Errorf("push to server: %w", err)
		}
	}
	fmt.Printf("%s: %d offers (%d cards)%s\n", b.ID, len(res.Offers), len(res.Cards),
		map[bool]string{true: "", false: " [PARTIAL — stale marked]"}[res.Complete])
	if verbose {
		for _, o := range res.Offers[:min(5, len(res.Offers))] {
			fmt.Printf("   %s | %s | %s | %s\n", o.CardLabel, o.Merchant, o.RewardText, o.Status)
		}
	}
	return nil
}

func cmdSync() *cobra.Command {
	return &cobra.Command{
		Use:   "sync [bank ...]",
		Short: "Run bank flows and collect offers",
		RunE: func(cmd *cobra.Command, args []string) error {
			banks, err := loadBanks()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			host, _ := os.Hostname()
			var errs []string
			for _, id := range argsOr(args, banks) {
				b, ok := banks[id]
				if !ok {
					errs = append(errs, fmt.Sprintf("%s: no such bank config", id))
					continue
				}
				if err := syncBank(ctx, b, host, true); err != nil {
					errs = append(errs, fmt.Sprintf("%s: %v", id, err))
				}
			}
			if len(errs) > 0 {
				return fmt.Errorf("%s", strings.Join(errs, "\n"))
			}
			return nil
		},
	}
}

func cmdWorker() *cobra.Command {
	var poll time.Duration
	var banksFlag []string
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Long-lived loop: claim jobs from a serve instance, run browsers, push results",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flagServer == "" {
				return fmt.Errorf("worker requires --server")
			}
			banks, err := loadBanks()
			if err != nil {
				return err
			}
			mine := banksFlag
			if len(mine) == 0 {
				mine = argsOr(nil, banks)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			host, _ := os.Hostname()
			c := engine.NewClient(flagServer, flagToken)
			fmt.Printf("worker %s serving banks %v (poll %v)\n", host, mine, poll)
			for ctx.Err() == nil {
				pr, err := c.PollJobs(ctx, host, mine, nil)
				if err != nil {
					fmt.Fprintf(os.Stderr, "poll: %v\n", err)
				} else if pr.Job != nil {
					b, ok := banks[pr.Job.Bank]
					if !ok {
						c.CompleteJob(ctx, pr.Job.ID, "error", "no config for bank")
						continue
					}
					runID, _ := c.StartRun(ctx, b.ID, host)
					br, err := engine.NewBrowser(b.Browser, b.BaseURL)
					if err != nil {
						c.CompleteJob(ctx, pr.Job.ID, "error", err.Error())
						continue
					}
					r := &engine.Runner{Bank: b, Browser: br, Log: os.Stderr,
						Gate: &engine.ServerGate{Client: c, RunID: runID, Worker: host, Banks: mine}}
					res := r.Run(ctx)
					br.Close()
					status, errMsg := "done", ""
					if res.Err != nil {
						status, errMsg = "error", res.Err.Error()
					} else if !res.Complete {
						status = "done" // partial: data not pushed; note in error
						errMsg = "partial: " + strings.Join(res.SkippedLog, "; ")
					}
					if res.Err == nil && len(res.Offers) > 0 {
						if err := c.Ingest(ctx, engine.IngestReq{
							Bank: b.ID, Worker: host, Complete: res.Complete,
							Cards: res.Cards, Offers: res.Offers,
						}); err != nil {
							status, errMsg = "error", "ingest: "+err.Error()
						}
					}
					c.FinishRun(ctx, runID, status, errMsg, len(res.Offers))
					c.CompleteJob(ctx, pr.Job.ID, status, errMsg)
					fmt.Printf("%s: job %d → %s (%d offers)\n", b.ID, pr.Job.ID, status, len(res.Offers))
				}
				select {
				case <-ctx.Done():
				case <-time.After(poll):
				}
			}
			return ctx.Err()
		},
	}
	cmd.Flags().DurationVar(&poll, "poll", 30*time.Second, "job poll interval")
	cmd.Flags().StringSliceVar(&banksFlag, "banks", nil, "banks this worker serves (default: all configs)")
	return cmd
}

func cmdServe() *cobra.Command {
	var listen, token string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Web UI + API (the always-on half)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := store.Open(flagDB)
			if err != nil {
				return err
			}
			defer st.Close()
			if token == "" {
				tokenPath := filepath.Join(filepath.Dir(flagDB), "token")
				if b, err := os.ReadFile(tokenPath); err == nil {
					token = strings.TrimSpace(string(b))
				}
			}
			if token == "" {
				token, _ = store.NewToken()
			}
			sv := web.New(st, token)
			fmt.Printf("offerhound serving http://%s\naccess token: %s\n", listen, token)
			return sv.Serve(listen)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8090", "host:port (0.0.0.0 exposes the LAN; token required)")
	cmd.Flags().StringVar(&token, "token", "", "API token (default: data dir token file)")
	return cmd
}

func cmdSearch() *cobra.Command {
	var bank, status, channel string
	var limit int
	cmd := &cobra.Command{
		Use:   "search <query...>",
		Short: "Search collected offers from the terminal",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(flagDB)
			if err != nil {
				return err
			}
			defer st.Close()
			res, err := st.Search(store.Query{
				Text: strings.Join(args, " "), Bank: bank, Status: status, Channel: channel, Limit: limit,
			})
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "CARD\tMERCHANT\tREWARD\tCHANNEL\tSTATUS\tEXPIRES\tNOTES")
			for _, o := range res {
				notes := ""
				if o.Notes != "" {
					notes = "•"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					o.CardLabel, o.Merchant, o.RewardText, o.Channel, o.Status, o.ExpiryDate, notes)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&bank, "bank", "", "filter by bank id")
	cmd.Flags().StringVar(&status, "status", "", "eligible|enrolled")
	cmd.Flags().StringVar(&channel, "channel", "", "in-store|online")
	cmd.Flags().IntVar(&limit, "limit", 50, "max results")
	return cmd
}

func argsOr(args []string, banks map[string]*config.Bank) []string {
	if len(args) > 0 {
		return args
	}
	ids := make([]string, 0, len(banks))
	for id := range banks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
