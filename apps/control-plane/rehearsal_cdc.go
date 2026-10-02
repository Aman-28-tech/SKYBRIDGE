// Rehearsal data plane: measured CDC convergence + reconciliation.
//
// Live implementation measures local PostgreSQL + Redpanda Pandaproxy with
// stdlib only (plus lib/pq, already a control-plane dependency). Tests inject
// scripted fakes through the rehearsalDataPlane seam.
//
// Reconciliation scope: the rehearsal probe set. Full-table comparison would
// flag pre-existing lab divergence unrelated to the rehearsal; counts are
// reported for context, probes are gated. IDs and field names are reported;
// values and PII never leave the databases.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	cdc "skybridge/cdc-applier"
)

// RehearsalTables is the authoritative set, matching the CDC contract.
var RehearsalTables = []string{"users", "products", "orders", "order_items"}

// fakeRehearsalDataPlane is the scripted test double. RunCalls counts
// executions (concurrency/determinism proofs). FailRun, when set, makes
// RunReplication fail (cleared by the test to prove resume).
type fakeRehearsalDataPlane struct {
	Report     CDCReport
	Recon      Reconciliation
	RunErr     error
	ReconErr   error
	RunCalls   int
	ReconCalls int
	LastSpec   RehearsalSpec
	FailTimes  int // fail the first FailTimes RunReplication calls, then succeed
}

func (f *fakeRehearsalDataPlane) RunReplication(ctx context.Context, spec RehearsalSpec) (CDCReport, error) {
	f.RunCalls++
	f.LastSpec = spec
	if f.FailTimes > 0 {
		f.FailTimes--
		return CDCReport{}, fmt.Errorf("fake transient cdc failure")
	}
	if f.RunErr != nil {
		return CDCReport{}, f.RunErr
	}
	return f.Report, nil
}

func (f *fakeRehearsalDataPlane) Reconcile(ctx context.Context, probeIDs []string) (Reconciliation, error) {
	f.ReconCalls++
	if f.ReconErr != nil {
		return Reconciliation{}, f.ReconErr
	}
	return f.Recon, nil
}

// cleanRecon builds a matching reconciliation over probeIDs (test helper).
func cleanRecon(probeCount int) Reconciliation {
	tables := make([]TableRecon, 0, len(RehearsalTables))
	for _, t := range RehearsalTables {
		tables = append(tables, TableRecon{
			Table: t, SourceCount: probeCount, TargetCount: probeCount,
			ProbesExpected: probeCount, ProbesMatched: probeCount,
			MissingIDs: []string{}, UnexpectedIDs: []string{}, Mismatched: []string{},
		})
	}
	return Reconciliation{Tables: tables, Match: true, Fingerprint: reconFingerprint(tables)}
}

func reconFingerprint(tables []TableRecon) string {
	var sb strings.Builder
	for _, t := range tables {
		miss := append([]string{}, t.MissingIDs...)
		unexp := append([]string{}, t.UnexpectedIDs...)
		mism := append([]string{}, t.Mismatched...)
		sort.Strings(miss)
		sort.Strings(unexp)
		sort.Strings(mism)
		fmt.Fprintf(&sb, "%s|%d|%d|%d|%d|%s|%s|%s;",
			t.Table, t.SourceCount, t.TargetCount, t.ProbesExpected, t.ProbesMatched,
			strings.Join(miss, ","), strings.Join(unexp, ","), strings.Join(mism, ","))
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])[:16]
}

// healthyCDCReport builds a within-RPO report (test helper).
func healthyCDCReport(srcLSN, appliedLSN string) CDCReport {
	return CDCReport{
		SourceLSN: srcLSN, AppliedLSN: appliedLSN, BaseLSN: "0/A000",
		ProbeIDs: []string{"probe-1", "probe-2"},
		SourceCommitUnix: 1700000000, ObserveUnix: 1700000005,
		LagSeconds: 5, WithinRPO: true,
		EventsCaptured: 2, EventsApplied: 2, EventsDuplicates: 1,
		CheckpointLSN: appliedLSN, TargetHealthy: true,
	}
}

// ---- live implementation ----

type liveDataPlane struct {
	srcURL   string
	tgtURL   string
	proxyURL string
	workload string
}

func liveRehearsalDataPlane() RehearsalDataPlane {
	return &liveDataPlane{
		srcURL:   envOr("CDC_SOURCE_DATABASE_URL", "postgres://cloudshop:cloudshop@localhost:5433/cloudshop?sslmode=disable"),
		tgtURL:   envOr("CDC_TARGET_DATABASE_URL", "postgres://cloudshop:cloudshop@localhost:5434/cloudshop?sslmode=disable"),
		proxyURL: envOr("REDPANDA_PROXY_URL", "http://localhost:8082"),
		workload: envOr("WORKLOAD_ID", "cloudshop"),
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func openDB(url string) (*sql.DB, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// RunReplication writes probe users to source, awaits their Debezium capture
// in Redpanda via Pandaproxy, applies each through the canonical cdc path
// (FromDebezium + PostgresStore.ApplyAtomically), replays the first event to
// prove duplicate safety, and measures lag from evidence.
func (l *liveDataPlane) RunReplication(ctx context.Context, spec RehearsalSpec) (CDCReport, error) {
	var rep CDCReport
	src, err := openDB(l.srcURL)
	if err != nil {
		return rep, fmt.Errorf("source database unavailable: %w", err)
	}
	defer src.Close()
	tgt, err := openDB(l.tgtURL)
	if err != nil {
		return rep, fmt.Errorf("target database unavailable: %w", err)
	}
	defer tgt.Close()
	rep.TargetHealthy = true

	var preLSN string
	if err := src.QueryRow(`SELECT pg_current_wal_lsn()::text`).Scan(&preLSN); err != nil {
		return rep, fmt.Errorf("source position: %w", err)
	}
	rep.BaseLSN = preLSN
	rep.ProbeIDs = append([]string{}, spec.ProbeIDs...)

	// Probes: deterministic IDs, unique tagged emails (identifiable, non-PII).
	// The email embeds the probe ID: re-execution of the same rehearsal key
	// reuses IDs (INSERT ... DO NOTHING), while distinct rehearsals never
	// collide on the users_email_key unique constraint.
	for _, pid := range spec.ProbeIDs {
		email := fmt.Sprintf("probe-%s@example.invalid", pid[:8])
		if _, err := src.Exec(`INSERT INTO users (id, email) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, pid, email); err != nil {
			return rep, fmt.Errorf("source probe write: %w", err)
		}
	}

	// Await capture of every probe in Redpanda.
	captured, err := l.awaitProbes(ctx, spec)
	if err != nil {
		return rep, err
	}
	rep.EventsCaptured = uint64(len(captured))

	// Apply through the proven canonical path.
	store := cdc.NewPostgresStore(tgt)
	if err := store.Ensure(); err != nil {
		return rep, fmt.Errorf("target cdc state: %w", err)
	}
	var applied, dups uint64
	var maxLSN string
	var maxCommit int64
	for _, rec := range captured {
		ev, err := cdc.FromDebezium(l.workload, rec)
		if err != nil {
			return rep, fmt.Errorf("canonical mapping: %w", err)
		}
		ok, err := store.ApplyAtomically(ev)
		if err != nil {
			return rep, fmt.Errorf("target apply: %w", err)
		}
		if ok {
			applied++
		} else {
			dups++
		}
		if maxLSN == "" {
			maxLSN = ev.LSN
		} else if c, err := cdc.CompareLSN(ev.LSN, maxLSN); err == nil && c > 0 {
			maxLSN = ev.LSN
		}
		if ev.SourceCommitMs/1000 > maxCommit {
			maxCommit = ev.SourceCommitMs / 1000
		}
	}
	// Duplicate-safety proof: replay the first captured event.
	if len(captured) > 0 {
		ev, err := cdc.FromDebezium(l.workload, captured[0])
		if err != nil {
			return rep, fmt.Errorf("replay mapping: %w", err)
		}
		ok, err := store.ApplyAtomically(ev)
		if err != nil {
			return rep, fmt.Errorf("replay apply: %w", err)
		}
		if ok {
			return rep, fmt.Errorf("replay unexpectedly applied: duplicate safety violated")
		}
		dups++
	}
	rep.EventsApplied = applied
	rep.EventsDuplicates = dups

	var postLSN string
	if err := src.QueryRow(`SELECT pg_current_wal_lsn()::text`).Scan(&postLSN); err != nil {
		return rep, fmt.Errorf("source post position: %w", err)
	}
	now := time.Now().Unix()
	rep.SourceLSN = postLSN
	rep.AppliedLSN = maxLSN
	rep.CheckpointLSN = maxLSN
	rep.SourceCommitUnix = maxCommit
	rep.ObserveUnix = now
	rep.LagSeconds = now - maxCommit
	if rep.LagSeconds < 0 {
		rep.LagSeconds = 0
	}
	rpoSeconds := spec.RPOSeconds
	if rpoSeconds <= 0 {
		rpoSeconds = 30
	}
	rep.WithinRPO = rep.LagSeconds <= int64(rpoSeconds)
	return rep, nil
}

// Reconcile compares the probe set between source and target. Only probe IDs
// gate Match; counts are context.
func (l *liveDataPlane) Reconcile(ctx context.Context, probeIDs []string) (Reconciliation, error) {
	src, err := openDB(l.srcURL)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("source database unavailable: %w", err)
	}
	defer src.Close()
	tgt, err := openDB(l.tgtURL)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("target database unavailable: %w", err)
	}
	defer tgt.Close()
	tables := make([]TableRecon, 0, len(RehearsalTables))
	match := true
	for _, t := range RehearsalTables {
		tr := TableRecon{Table: t, MissingIDs: []string{}, UnexpectedIDs: []string{}, Mismatched: []string{}}
		if err := ctx.Err(); err != nil {
			return Reconciliation{}, err
		}
		srcRows, err := readTableIDs(ctx, src, t)
		if err != nil {
			return Reconciliation{}, fmt.Errorf("source %s: %w", t, err)
		}
		tgtRows, err := readTableIDs(ctx, tgt, t)
		if err != nil {
			return Reconciliation{}, fmt.Errorf("target %s: %w", t, err)
		}
		tr.SourceCount, tr.TargetCount = len(srcRows), len(tgtRows)
		probeSet := map[string]bool{}
		for _, p := range probeIDs {
			probeSet[p] = true
		}
		// Probes only exist in users; other tables report counts.
		if t == "users" {
			tr.ProbesExpected = len(probeIDs)
			for _, p := range probeIDs {
				sv, inSrc := srcRows[p]
				tv, inTgt := tgtRows[p]
				switch {
				case !inSrc:
					tr.MissingIDs = append(tr.MissingIDs, p+":source")
					match = false
				case !inTgt:
					tr.MissingIDs = append(tr.MissingIDs, p+":target")
					match = false
				case sv != tv:
					tr.Mismatched = append(tr.Mismatched, p)
					match = false
				default:
					tr.ProbesMatched++
				}
			}
		}
		sort.Strings(tr.MissingIDs)
		sort.Strings(tr.UnexpectedIDs)
		sort.Strings(tr.Mismatched)
		tables = append(tables, tr)
	}
	return Reconciliation{Tables: tables, Match: match, Fingerprint: reconFingerprint(tables)}, nil
}

// readTableIDs returns id -> field fingerprint (values hashed, never returned).
func readTableIDs(ctx context.Context, db *sql.DB, table string) (map[string]string, error) {
	var rows *sql.Rows
	var err error
	switch table {
	case "users":
		rows, err = db.QueryContext(ctx, `SELECT id::text, email FROM users`)
	case "products":
		rows, err = db.QueryContext(ctx, `SELECT id::text, sku, name, price_cents::text FROM products`)
	case "orders":
		rows, err = db.QueryContext(ctx, `SELECT id::text, user_id::text, status, total_cents::text FROM orders`)
	case "order_items":
		rows, err = db.QueryContext(ctx, `SELECT id::text, order_id::text, product_id::text, quantity::text, unit_price_cents::text FROM order_items`)
	default:
		return nil, fmt.Errorf("unknown table %q", table)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := map[string]string{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		strs := make([]string, len(vals))
		for i, v := range vals {
			strs[i] = fmt.Sprint(v)
		}
		sum := sha256.Sum256([]byte(strings.Join(strs[1:], "|")))
		out[strs[0]] = hex.EncodeToString(sum[:])[:16]
	}
	return out, rows.Err()
}

// ---- Pandaproxy polling (stdlib; no Kafka client) ----

func (l *liveDataPlane) awaitProbes(ctx context.Context, spec RehearsalSpec) ([][]byte, error) {
	group := "rehearsal-" + spec.RehearsalKey
	if len(group) > 64 {
		sum := sha256.Sum256([]byte(group))
		group = "rehearsal-" + hex.EncodeToString(sum[:])[:16]
	}
	instance, baseURI, err := l.createConsumer(ctx, group)
	if err != nil {
		return nil, fmt.Errorf("redpanda consumer: %w", err)
	}
	defer l.deleteConsumer(baseURI)
	if err := l.subscribe(ctx, baseURI, []string{"cloudshop.public.users"}); err != nil {
		return nil, fmt.Errorf("redpanda subscribe: %w", err)
	}
	_ = instance
	want := map[string]bool{}
	for _, p := range spec.ProbeIDs {
		want[p] = false
	}
	var out [][]byte
	deadline := time.Now().Add(spec.Timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		recs, err := l.fetchRecords(ctx, baseURI)
		if err != nil {
			return nil, fmt.Errorf("redpanda fetch: %w", err)
		}
		for _, r := range recs {
			var env struct {
				After *map[string]any `json:"after"`
			}
			if err := json.Unmarshal(r, &env); err != nil || env.After == nil {
				continue
			}
			id, _ := (*env.After)["id"].(string)
			if seen, ok := want[id]; ok && !seen {
				want[id] = true
				out = append(out, r)
			}
		}
		done := true
		for _, s := range want {
			if !s {
				done = false
			}
		}
		if done {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	missing := []string{}
	for id, s := range want {
		if !s {
			missing = append(missing, id)
		}
	}
	return nil, fmt.Errorf("capture timeout: %d/%d probes captured, missing %d", len(out), len(want), len(missing))
}

func proxyDo(ctx context.Context, method, url string, body any) ([]byte, int, error) {
	return proxyDoAccept(ctx, method, url, body, "application/vnd.kafka.v2+json")
}

func proxyDoAccept(ctx context.Context, method, url string, body any, accept string) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/vnd.kafka.v2+json")
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func (l *liveDataPlane) createConsumer(ctx context.Context, group string) (string, string, error) {
	instance := fmt.Sprintf("rehearsal-%d", time.Now().UnixNano()%1000000)
	url := strings.TrimRight(l.proxyURL, "/") + "/consumers/" + group
	b, code, err := proxyDo(ctx, "POST", url, map[string]any{
		"name": instance, "format": "binary", "auto.offset.reset": "earliest",
	})
	if err != nil {
		return "", "", err
	}
	if code != 200 && code != 201 && code != 409 {
		return "", "", fmt.Errorf("create consumer: status %d: %s", code, string(b))
	}
	var out struct {
		BaseURI string `json:"base_uri"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.BaseURI == "" {
		// 409 (already exists): derive instance URI conventionally.
		out.BaseURI = url + "/instances/" + instance
	}
	// The broker advertises its in-docker Pandaproxy address
	// (redpanda:8082) in base_uri, which is unresolvable from the host.
	// Re-anchor the returned path to the configured proxy endpoint.
	out.BaseURI = reanchorBaseURI(l.proxyURL, out.BaseURI)
	return instance, out.BaseURI, nil
}

// reanchorBaseURI keeps the broker-returned path but points it at the
// configured proxy endpoint (scheme + host), so host-run and in-cluster
// callers both work regardless of the advertised address.
func reanchorBaseURI(proxyURL, baseURI string) string {
	pu, err := url.Parse(strings.TrimRight(proxyURL, "/"))
	if err != nil {
		return baseURI
	}
	bu, err := url.Parse(baseURI)
	if err != nil {
		return baseURI
	}
	bu.Scheme = pu.Scheme
	bu.Host = pu.Host
	return bu.String()
}

func (l *liveDataPlane) subscribe(ctx context.Context, baseURI string, topics []string) error {
	b, code, err := proxyDo(ctx, "POST", strings.TrimRight(baseURI, "/")+"/subscription", map[string]any{"topics": topics})
	if err != nil {
		return err
	}
	if code != 200 && code != 201 && code != 204 {
		return fmt.Errorf("subscribe: status %d: %s", code, string(b))
	}
	return nil
}

func (l *liveDataPlane) fetchRecords(ctx context.Context, baseURI string) ([][]byte, error) {
	// Records require the format-specific accept type (binary was negotiated
	// at consumer creation); the generic v2 type yields 406 here.
	b, code, err := proxyDoAccept(ctx, "GET", strings.TrimRight(baseURI, "/")+"/records?timeout=8000&max_bytes=1048576", nil, "application/vnd.kafka.binary.v2+json")
	if err != nil {
		return nil, err
	}
	if code == 404 {
		return nil, nil
	}
	if code != 200 {
		return nil, fmt.Errorf("fetch: status %d: %s", code, string(b))
	}
	var recs []struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(recs))
	for _, r := range recs {
		raw, err := base64.StdEncoding.DecodeString(r.Value)
		if err != nil {
			continue
		}
		out = append(out, raw)
	}
	return out, nil
}

func (l *liveDataPlane) deleteConsumer(baseURI string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, _ = proxyDo(ctx, "DELETE", baseURI, nil)
}
