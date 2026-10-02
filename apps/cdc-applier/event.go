// Canonical CDC event contract for the SKYBRIDGE replication applier.
//
// Source ordering authority is the PostgreSQL LSN / WAL position, never
// wall-clock arrival time. EventID is deterministic over source identity.
//
// Debezium field mapping (PostgresConnector, pgoutput, JsonConverter without
// schemas). The connector emits a Kafka record per row change:
//
//	{
//	  "before": {...}|null, "after": {...}|null,
//	  "source": {"db":"cloudshop","table":"orders","lsn":"0/16B1978",
//	              "txId": 512, "ts_ms": 1727...},
//	  "op": "c"|"u"|"d"|"r", "ts_ms": 1727...,
//	  "transaction": {"id": "512:...", "total_order": 1, "data_collection_order": 1}|null
//	}
//
// Mapping (see FromDebezium):
//   - table            <- payload.source.table
//   - source_db        <- payload.source.db
//   - lsn              <- payload.source.lsn (string "0/..." form preserved)
//   - txn_id           <- payload.source.txId (preferred; fallback transaction.id)
//   - source_commit_ms <- payload.source.ts_ms (commit time, NOT envelope ts_ms)
//   - op               <- payload.op, strictly {c,u,d,r} (Debezium "r" = snapshot read)
//   - before/after     <- payload.before / payload.after
//   - pk               <- after.id else before.id (CloudShop v1: all PKs are "id" UUID)
//   - workload_id      <- applier config (topic prefix mapping), not the payload
//   - schema_version   <- applier-known CloudShop schema version (default 1)
//
// Dropped/never trusted: envelope ts_ms (processing time), Kafka offsets,
// Redpanda arrival time. Those must not influence ordering or identity.
//
// Dedupe semantics (Option A, see SEMANTICS below):
//   - cdc_applied_events(event_id) is the AUTHORITATIVE per-event dedupe.
//   - cdc_applied_txn(txn_id) is an observability/completion marker only and
//     is NEVER consulted to skip an event. A transaction T1 with E1,E2,E3 must
//     apply all three; checking txn_id after E1 would corrupt E2/E3.
//   - cdc_offsets(consumer) checkpoint advances only past safely applied LSNs.
package cdc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CloudShop schema version replicated by this applier.
const cdcSchemaVersion = 1

// Authoritative business tables. Anything else is rejected by Validate.
// idempotency_keys, applied_jobs, schema_version, cdc_* are infrastructure and
// must never be replicated as business state.
var allowedTables = map[string]bool{
	"users":       true,
	"products":    true,
	"orders":      true,
	"order_items": true,
}

// Valid operations: c=insert, u=update, d=delete, r=snapshot read (treated as upsert).
var allowedOps = map[string]bool{"c": true, "u": true, "d": true, "r": true}

// CDCEvent is the canonical unit of application. One event == one row change.
type CDCEvent struct {
	WorkloadID string `json:"workload_id"`
	SourceDB   string `json:"source_db"`
	Table      string `json:"table"`
	Op         string `json:"op"`
	PK         string `json:"pk"`
	LSN        string `json:"lsn"`
	// SourceCommitMs is payload.source.ts_ms (commit time). Used for lag math.
	SourceCommitMs int64 `json:"source_commit_ms"`
	// TxnID is payload.source.txId. Groups events; never used to skip events.
	TxnID         string         `json:"txn_id"`
	SchemaVersion int            `json:"schema_version"`
	Before        map[string]any `json:"before,omitempty"`
	After         map[string]any `json:"after,omitempty"`
}

// Validate enforces strict decoding: unknown table/op, missing pk/lsn are errors.
func (e *CDCEvent) Validate() error {
	if e.WorkloadID == "" {
		return fmt.Errorf("CDC event missing workload_id")
	}
	if e.SourceDB == "" {
		return fmt.Errorf("CDC event missing source_db")
	}
	if !allowedTables[e.Table] {
		return fmt.Errorf("CDC event table not replicated: %q", e.Table)
	}
	if !allowedOps[e.Op] {
		return fmt.Errorf("CDC event unknown op: %q", e.Op)
	}
	if e.PK == "" {
		return fmt.Errorf("CDC event missing pk")
	}
	if e.LSN == "" {
		return fmt.Errorf("CDC event missing lsn")
	}
	if e.SourceCommitMs <= 0 {
		return fmt.Errorf("CDC event missing source_commit_ms")
	}
	switch e.Op {
	case "c", "r":
		if len(e.After) == 0 {
			return fmt.Errorf("CDC event op %q requires after payload", e.Op)
		}
	case "u":
		if len(e.After) == 0 && len(e.Before) == 0 {
			return fmt.Errorf("CDC event op u requires before or after payload")
		}
	case "d":
		if len(e.Before) == 0 && len(e.After) == 0 {
			return fmt.Errorf("CDC event op d requires before payload")
		}
	}
	return nil
}

// EventID is deterministic over source ordering identity, never wall-clock.
// Format: hex(sha256(workload|db|table|op|pk|lsn|txn|schema|afterPK)).
//
// LSN is the ordering authority: the same row re-committed at a new LSN is a
// new event; the same event redelivered has the same LSN and hence same ID.
func (e *CDCEvent) EventID() string {
	afterKeys := ""
	if len(e.After) > 0 {
		keys := make([]string, 0, len(e.After))
		for k := range e.After {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&sb, "%s=%v;", k, e.After[k])
		}
		sum := sha256.Sum256([]byte(sb.String()))
		afterKeys = hex.EncodeToString(sum[:])[:16]
	}
	raw := strings.Join([]string{
		e.WorkloadID, e.SourceDB, e.Table, e.Op, e.PK, e.LSN, e.TxnID,
		fmt.Sprint(e.SchemaVersion), afterKeys,
	}, "|")
	sum := sha256.Sum256([]byte(raw))
	return "evt_" + hex.EncodeToString(sum[:])[:32]
}

// MarshalCanonical serializes deterministically (struct field order is fixed).
func (e *CDCEvent) MarshalCanonical() ([]byte, error) {
	return json.Marshal(e)
}

// UnmarshalCDCEvent decodes strictly: unknown fields rejected, then Validate.
func UnmarshalCDCEvent(b []byte) (*CDCEvent, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var e CDCEvent
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("CDC event malformed: %w", err)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

// debeziumEnvelope mirrors the connector JSON value (schemas disabled).
// NOTE: source.lsn arrives as a JSON number (WAL integer) in pgoutput mode,
// not the "0/..." display string. LSNString normalizes both forms.
type debeziumEnvelope struct {
	Before *map[string]any `json:"before"`
	After  *map[string]any `json:"after"`
	Op     string          `json:"op"`
	Source *struct {
		DB    string `json:"db"`
		Table string `json:"table"`
		LSN   any    `json:"lsn"`
		TxID  any    `json:"txId"`
		TsMs  int64  `json:"ts_ms"`
	} `json:"source"`
	Transaction *struct {
		ID string `json:"id"`
	} `json:"transaction"`
}

// normalizeLSN converts a Debezium source.lsn (number or "0/..." string) to
// the canonical "0/<UPPERHEX>" display form used for ordering/identity.
func normalizeLSN(v any) string {
	switch t := v.(type) {
	case string:
		if t != "" {
			return t
		}
		return ""
	case float64:
		if t < 0 {
			return ""
		}
		return "0/" + strings.ToUpper(strconv.FormatUint(uint64(t), 16))
	case int64:
		if t < 0 {
			return ""
		}
		return "0/" + strings.ToUpper(strconv.FormatUint(uint64(t), 16))
	default:
		return stringify(v)
	}
}

func stringify(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprint(int64(t))
		}
		return fmt.Sprint(t)
	default:
		return fmt.Sprint(v)
	}
}

func pkOf(table string, after, before map[string]any) string {
	if after != nil {
		if id, ok := after["id"]; ok && stringify(id) != "" {
			return stringify(id)
		}
	}
	if before != nil {
		if id, ok := before["id"]; ok {
			return stringify(id)
		}
	}
	return ""
}

// FromDebezium maps a connector record to the canonical event.
// workloadID comes from applier config (topic-prefix mapping), not the payload.
func FromDebezium(workloadID string, record []byte) (*CDCEvent, error) {
	var env debeziumEnvelope
	if err := json.Unmarshal(record, &env); err != nil {
		return nil, fmt.Errorf("CDC event malformed debezium envelope: %w", err)
	}
	if env.Source == nil {
		return nil, fmt.Errorf("CDC event missing source block")
	}
	var before, after map[string]any
	if env.Before != nil {
		before = *env.Before
	}
	if env.After != nil {
		after = *env.After
	}
	txn := stringify(env.Source.TxID)
	if txn == "" && env.Transaction != nil {
		txn = env.Transaction.ID
	}
	e := &CDCEvent{
		WorkloadID:     workloadID,
		SourceDB:       env.Source.DB,
		Table:          env.Source.Table,
		Op:             env.Op,
		PK:             pkOf(env.Source.Table, after, before),
		LSN:            normalizeLSN(env.Source.LSN),
		SourceCommitMs: env.Source.TsMs,
		TxnID:          txn,
		SchemaVersion:  cdcSchemaVersion,
		Before:         before,
		After:          after,
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}
