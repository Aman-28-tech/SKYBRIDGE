// cdc-catchup: one-shot deterministic drain of pending CDC topic records
// into the target PostgreSQL through the canonical apply path.
//
// Local demo path: cloudshop-db :5433 (WAL) -> Debezium -> Redpanda topics
// -> this tool (FromDebezium + PostgresStore.ApplyAtomically, per-event
// idempotent dedupe) -> cloudshop-target-db :5434.
//
// This is the same proven canonical path the control-plane rehearsal uses
// for probes, extended to every pending record on the workload topics so
// demo traffic (users + orders) is really replicated before final
// validation. Metrics are measured (captured/applied/duplicates/LSN),
// never asserted.
//
// Configuration (env only, no hardcoded secrets/paths):
//   REDPANDA_PROXY_URL     Pandaproxy endpoint (default http://localhost:8082)
//   CDC_TARGET_DATABASE_URL target Postgres URL (default local target :5434)
//   WORKLOAD_ID            workload identity (default "cloudshop")
//   CATCHUP_GROUP          consumer group (default "catchup-<unixnano>");
//                          a fresh group reads the full backlog from earliest.
//   CATCHUP_TOPICS         comma-separated topics (default users+orders;
//                          widen explicitly; products/order_items carry
//                          pre-existing seed divergence, see below)
//   CATCHUP_MAX_POLLS      upper poll bound (default 60)
//   CATCHUP_IDLE_EXIT      consecutive empty polls before exit (default 3)
//
// Safety: read-only against Redpanda, idempotent writes to the local target
// only. No cloud APIs, no Terraform, no ownership changes.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"

	cdc "skybridge/cdc-applier"
)

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "CATCHUP_FAIL: "+format+"\n", args...)
	os.Exit(1)
}

func proxyDo(ctx context.Context, method, target string, body any, accept string) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
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

const v2Type = "application/vnd.kafka.v2+json"
const binType = "application/vnd.kafka.binary.v2+json"

// reanchorBaseURI keeps the broker-returned path but points it at the
// configured proxy endpoint (the broker advertises in-docker redpanda:8082,
// unresolvable from the host).
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

func topicTable(topic string) string {
	if i := strings.LastIndex(topic, "."); i >= 0 {
		return topic[i+1:]
	}
	return topic
}

// orderTopics sorts drain phases by referential dependency (parents before
// children) so a users row is always applied before any order referencing
// it. Unknown tables keep input order at the end; empties are dropped.
func orderTopics(topics []string) []string {
	ordered := []string{}
	seen := map[string]bool{}
	for _, table := range []string{"users", "products", "orders", "order_items"} {
		for _, t := range topics {
			t = strings.TrimSpace(t)
			if t == "" || seen[t] {
				continue
			}
			if topicTable(t) == table {
				ordered = append(ordered, t)
				seen[t] = true
			}
		}
	}
	for _, t := range topics {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			ordered = append(ordered, t)
			seen[t] = true
		}
	}
	return ordered
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	proxyURL := getenv("REDPANDA_PROXY_URL", "http://localhost:8082")
	targetURL := getenv("CDC_TARGET_DATABASE_URL",
		"postgres://cloudshop:cloudshop@localhost:5434/cloudshop?sslmode=disable")
	workload := getenv("WORKLOAD_ID", "cloudshop")
	group := getenv("CATCHUP_GROUP", fmt.Sprintf("catchup-%d", time.Now().UnixNano()))
	// Default scope is the demo traffic tables (users, orders): the two lab
	// databases were seeded independently with clashing products SKUs under
	// different IDs, so products/order_items replay hits a pre-existing
	// unique-constraint divergence unrelated to any migration traffic.
	// Reconciliation is probe-scoped for exactly this documented reason;
	// override CATCHUP_TOPICS explicitly to widen the drain.
	topics := strings.Split(getenv("CATCHUP_TOPICS",
		"cloudshop.public.users,cloudshop.public.orders"), ",")
	// Phase order follows referential dependency (parents before children):
	// a cross-topic fetch can otherwise deliver an order before its user
	// and trip the target FK constraint. Each phase drains one topic with
	// its own consumer group, always from earliest.
	topics = orderTopics(topics)
	maxPolls := 60
	if v := os.Getenv("CATCHUP_MAX_POLLS"); v != "" {
		fmt.Sscanf(v, "%d", &maxPolls)
	}
	idleExit := 3
	if v := os.Getenv("CATCHUP_IDLE_EXIT"); v != "" {
		fmt.Sscanf(v, "%d", &idleExit)
	}

	db, err := sql.Open("postgres", targetURL)
	if err != nil {
		fail("target open: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		fail("target unreachable: %v", err)
	}
	store := cdc.NewPostgresStore(db)
	if err := store.Ensure(); err != nil {
		fail("target cdc state: %v", err)
	}

	var captured, applied, dups, skipped uint64
	var maxLSN string
	for _, topic := range topics {
		phaseGroup := group + "-" + topicTable(topic)
		instance := fmt.Sprintf("catchup-%d", time.Now().UnixNano()%1000000)
		createURL := strings.TrimRight(proxyURL, "/") + "/consumers/" + phaseGroup
		b, code, err := proxyDo(ctx, "POST", createURL, map[string]any{
			"name": instance, "format": "binary", "auto.offset.reset": "earliest",
		}, v2Type)
		if err != nil {
			fail("redpanda consumer: %v", err)
		}
		if code != 200 && code != 201 && code != 409 {
			fail("create consumer: status %d: %s", code, string(b))
		}
		var created struct {
			BaseURI string `json:"base_uri"`
		}
		if err := json.Unmarshal(b, &created); err != nil || created.BaseURI == "" {
			created.BaseURI = createURL + "/instances/" + instance
		}
		baseURI := reanchorBaseURI(proxyURL, created.BaseURI)

		b, code, err = proxyDo(ctx, "POST", strings.TrimRight(baseURI, "/")+"/subscription",
			map[string]any{"topics": []string{topic}}, v2Type)
		if err != nil {
			fail("redpanda subscribe: %v", err)
		}
		if code != 200 && code != 201 && code != 204 {
			fail("subscribe: status %d: %s", code, string(b))
		}

		idle := 0
		for poll := 0; poll < maxPolls; poll++ {
			if err := ctx.Err(); err != nil {
				fail("context: %v", err)
			}
			b, code, err := proxyDo(ctx, "GET",
				strings.TrimRight(baseURI, "/")+"/records?timeout=8000&max_bytes=1048576",
				nil, binType)
			if err != nil {
				fail("redpanda fetch: %v", err)
			}
			if code == 404 {
				idle++
				if idle >= idleExit {
					break
				}
				continue
			}
			if code != 200 {
				fail("fetch: status %d: %s", code, string(b))
			}
			var recs []struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(b, &recs); err != nil {
				fail("decode records: %v", err)
			}
			if len(recs) == 0 {
				idle++
				if idle >= idleExit {
					break
				}
				continue
			}
			idle = 0
			for _, r := range recs {
				raw, err := base64.StdEncoding.DecodeString(r.Value)
				if err != nil {
					skipped++
					continue
				}
				// Tombstones and delete envelopes (after == null) carry no
				// row image: reset-demo.sh mirrors demo deletes on both
				// databases directly, so there is nothing to apply. This
				// mirrors the rehearsal probe filter, which only admits
				// records with a usable after-image.
				var env struct {
					After *map[string]any `json:"after"`
				}
				if err := json.Unmarshal(raw, &env); err != nil || env.After == nil {
					skipped++
					continue
				}
				if id, _ := (*env.After)["id"].(string); id == "" {
					skipped++
					continue
				}
				ev, err := cdc.FromDebezium(workload, raw)
				if err != nil {
					skipped++
					continue
				}
				captured++
				ok, err := store.ApplyAtomically(ev)
				if err != nil {
					fail("target apply: %v", err)
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
			}
		}

		func() {
			dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer dcancel()
			_, _, _ = proxyDo(dctx, "DELETE", baseURI, nil, v2Type)
		}()
		fmt.Fprintf(os.Stderr, "phase %s drained\n", topic)
	}

	fmt.Printf("CATCHUP_OK captured=%d applied=%d duplicates=%d skipped=%d max_lsn=%s topics=%s\n",
		captured, applied, dups, skipped, maxLSN, strings.Join(topics, ","))
}
