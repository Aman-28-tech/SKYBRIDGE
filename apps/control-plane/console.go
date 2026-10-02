// Console read-only aggregation: dashboard + detail views for the Next.js
// console. All handlers are pure reads: no state mutation, no audit writes,
// no idempotency records, no policy evaluation, no cloud calls. Every
// "completed" claim derives from stored evidence (reports, records, audit
// entries); unknown stays explicit so the UI can never show fake success.
//
// Endpoints:
//   GET /v1/workloads/{id}/migrations   list migrations for a workload
//   GET /v1/migrations/{id}/summary     aggregated lifecycle/CDC/safety view
package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Lifecycle stages shown in the console (presentation order).
var consoleLifecycleStages = []string{
	"REGISTERED", "COMPATIBILITY", "PLANNED", "REHEARSED",
	"CANARY", "QUIESCED", "CUTOVER", "COMPLETE",
}

// Cutover stages shown in the console (execution order).
var consoleCutoverStages = []string{
	"FINAL_PREFLIGHT", "WRITES_QUIESCED", "CDC_CATCHING_UP", "CDC_CAUGHT_UP",
	"FINAL_VALIDATION", "OWNERSHIP_TRANSFERRED", "TRAFFIC_SWITCHED",
	"WRITES_RESUMED", "CUTOVER_COMPLETE",
}

type consoleStageView struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // completed|current|pending
	Timestamp string `json:"timestamp,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type consoleCutoverStageView struct {
	Name        string `json:"name"`
	Status      string `json:"status"` // completed|current|pending|blocked
	Timestamp   string `json:"timestamp,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	Actor       string `json:"actor,omitempty"`
	PolicyHash  string `json:"policy_hash,omitempty"`
	ApprovalID  string `json:"approval_id,omitempty"`
	SourceLSN   string `json:"source_lsn,omitempty"`
	AppliedLSN  string `json:"applied_lsn,omitempty"`
	LagSeconds  *int64 `json:"lag_seconds,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

func metaJSON(meta string) map[string]any {
	out := map[string]any{}
	if meta == "" {
		return out
	}
	_ = json.Unmarshal([]byte(meta), &out)
	return out
}

func metaStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func metaInt(m map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return int64(n), true
			case int64:
				return n, true
			case int:
				return int64(n), true
			}
		}
	}
	return 0, false
}

func metaUint(m map[string]any, keys ...string) (uint64, bool) {
	if v, ok := metaInt(m, keys...); ok && v >= 0 {
		return uint64(v), true
	}
	return 0, false
}

// latestAuditByAction indexes the migration audit by action (last wins).
func latestAuditByAction(audits []AuditEntry) map[string]AuditEntry {
	out := map[string]AuditEntry{}
	for _, a := range audits {
		out[a.Action] = a
	}
	return out
}

func auditSuccess(a AuditEntry) bool {
	return a.Result == "success"
}

// rpoSeconds reads requirements.rpo_seconds from the workload spec.
func rpoSeconds(wl map[string]any) int64 {
	if wl == nil {
		return 30
	}
	spec, _ := wl["canonical_spec"].(map[string]any)
	if spec == nil {
		return 30
	}
	req, _ := spec["requirements"].(map[string]any)
	if req == nil {
		return 30
	}
	if v, ok := req["rpo_seconds"].(float64); ok && v > 0 {
		return int64(v)
	}
	return 30
}

// listMigrationsForWorkload implements GET /v1/workloads/{id}/migrations.
func listMigrationsForWorkload(w http.ResponseWriter, r *http.Request, workloadID string) {
	rid := reqID(r)
	if _, ok := store.GetWorkload(workloadID); !ok {
		writeErr(w, rid, "NOT_FOUND", "workload not found", 404)
		return
	}
	items := store.ListMigrations(workloadID)
	if items == nil {
		items = []map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"workload_id": workloadID, "items": items, "request_id": rid,
	})
}

// getMigrationSummary implements GET /v1/migrations/{id}/summary.
func getMigrationSummary(w http.ResponseWriter, r *http.Request, migrationID string) {
	rid := reqID(r)
	mig, ok := store.GetMigration(migrationID)
	if !ok {
		writeErr(w, rid, "NOT_FOUND", "migration not found", 404)
		return
	}
	wid, _ := mig["workload_id"].(string)
	wl, _ := store.GetWorkload(wid)
	audits := store.GetAuditForMigration(migrationID)
	byAction := latestAuditByAction(audits)

	rep, hasCompat := store.GetLatestCompatReport(wid)
	plan, hasPlan := store.GetLatestTargetPlan(wid)
	var drift DriftReport
	hasDrift := false
	if hasPlan {
		drift, hasDrift = store.GetLatestDriftReport(wid, plan.ID)
	}
	ownership, hasOwnership := store.GetOwnership(migrationID)
	canaryRows := store.GetCanaryRecords(migrationID)
	approvals := store.GetApprovals(wid, ApprovalApproved)

	lifecycle := buildLifecycle(mig, byAction, hasCompat, hasPlan, migrationID)
	cutover := buildCutoverTimeline(byAction)
	cdc := buildCDCView(wl, ownership, hasOwnership, byAction)
	ownView := buildOwnershipView(ownership, hasOwnership)
	safety := buildSafetyView(rep, hasCompat, drift, hasDrift, byAction, approvals, canaryRows, ownership, hasOwnership)

	owner := "aws"
	routing := "aws"
	if hasOwnership {
		owner, routing = ownership.CurrentOwner, ownership.Routing
	}
	wname := ""
	if wl != nil {
		wname, _ = wl["name"].(string)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"migration_id": migrationID, "workload_id": wid,
		"migration":    mig,
		"workload":     map[string]any{"id": wid, "name": wname},
		"providers": map[string]any{
			"source_provider": "aws", "target_provider": "azure",
			"authoritative_provider": owner, "routing_provider": routing,
		},
		"mode": map[string]any{
			"local_demo": true, "real_aws": "disabled",
			"real_azure": "disabled", "terraform_apply": "disabled",
		},
		"lifecycle": lifecycle,
		"cutover":   cutover,
		"cdc":       cdc,
		"ownership": ownView,
		"safety":    safety,
		"request_id": rid,
	})
}

func buildLifecycle(mig map[string]any, byAction map[string]AuditEntry, hasCompat, hasPlan bool, migrationID string) map[string]any {
	done := map[string]consoleStageView{}
	mark := func(name, status, ts, detail string) {
		done[name] = consoleStageView{Name: name, Status: status, Timestamp: ts, Detail: detail}
	}
	// REGISTERED: migration record exists.
	ts := ""
	if a, ok := byAction["create_migration"]; ok {
		ts = a.CreatedAt
	}
	mark("REGISTERED", "completed", ts, "migration registered")

	// COMPATIBILITY
	if hasCompat {
		mark("COMPATIBILITY", "completed", "", "compatibility evaluated")
	} else {
		mark("COMPATIBILITY", "pending", "", "")
	}
	// PLANNED
	if hasPlan {
		mark("PLANNED", "completed", "", "target plan generated")
	} else {
		mark("PLANNED", "pending", "", "")
	}
	// REHEARSED: rehearsal_complete success, or rehearse_migration success
	// whose metadata reports REHEARSAL_READY.
	rehearsed := false
	var rehearsedTS string
	if a, ok := byAction["rehearsal_complete"]; ok && auditSuccess(a) {
		rehearsed, rehearsedTS = true, a.CreatedAt
	} else if a, ok := byAction["rehearse_migration"]; ok && auditSuccess(a) {
		if strings.Contains(a.Metadata, "REHEARSAL_READY") {
			rehearsed, rehearsedTS = true, a.CreatedAt
		}
	}
	if rehearsed {
		mark("REHEARSED", "completed", rehearsedTS, "rehearsal ready")
	} else {
		mark("REHEARSED", "pending", "", "")
	}
	// CANARY: stages 1/5/25/50 PASS.
	if canaryPassForStages(migrationID, []int{1, 5, 25, 50}) {
		mark("CANARY", "completed", "", "stages 1/5/25/50 PASS")
	} else {
		mark("CANARY", "pending", "", "")
	}
	// QUIESCED
	quiesced := false
	var quiescedTS string
	if a, ok := byAction["final_quiesce"]; ok && auditSuccess(a) {
		quiesced, quiescedTS = true, a.CreatedAt
	} else if a, ok := byAction["set_quiesce"]; ok && auditSuccess(a) && strings.Contains(a.Metadata, `"quiesced":true`) {
		quiesced, quiescedTS = true, a.CreatedAt
	}
	if quiesced {
		mark("QUIESCED", "completed", quiescedTS, "writes quiesced")
	} else {
		mark("QUIESCED", "pending", "", "")
	}
	// CUTOVER + COMPLETE
	transferred := false
	if a, ok := byAction["transfer_ownership"]; ok && auditSuccess(a) {
		transferred = true
		_ = transferred
		mark("CUTOVER", "completed", a.CreatedAt, "ownership transferred")
	} else if _, ok := byAction["final_preflight"]; ok {
		mark("CUTOVER", "current", "", "cutover in progress")
	} else {
		mark("CUTOVER", "pending", "", "")
	}
	if a, ok := byAction["cutover_complete"]; ok && auditSuccess(a) {
		mark("COMPLETE", "completed", a.CreatedAt, "cutover complete")
	} else {
		mark("COMPLETE", "pending", "", "")
	}

	stages := make([]consoleStageView, 0, len(consoleLifecycleStages))
	current := "REGISTERED"
	foundCurrent := false
	for _, name := range consoleLifecycleStages {
		sv := done[name]
		sv.Name = name
		if sv.Status == "" {
			sv.Status = "pending"
		}
		stages = append(stages, sv)
	}
	// Current = first non-completed; all completed -> COMPLETE.
	for _, s := range stages {
		if s.Status != "completed" {
			current = s.Name
			foundCurrent = true
			break
		}
	}
	if !foundCurrent {
		current = "COMPLETE"
	}
	// Mark the current stage explicitly (unless everything completed).
	out := make([]map[string]any, 0, len(stages))
	for _, s := range stages {
		status := s.Status
		if s.Name == current && status == "pending" {
			status = "current"
		}
		out = append(out, map[string]any{
			"name": s.Name, "status": status,
			"timestamp": s.Timestamp, "detail": s.Detail,
		})
	}
	_ = mig
	return map[string]any{"current": current, "stages": out}
}

func cutoverStageFromAudit(name string, a AuditEntry, ok bool) consoleCutoverStageView {
	v := consoleCutoverStageView{Name: name, Status: "pending"}
	if !ok {
		return v
	}
	m := metaJSON(a.Metadata)
	if auditSuccess(a) {
		v.Status = "completed"
	} else if a.Result == "failure" || a.Result == "denied" {
		v.Status = "blocked"
	} else {
		v.Status = "current"
	}
	v.Timestamp = a.CreatedAt
	v.RequestID = a.RequestID
	v.Actor = a.ActorID
	if h := metaStr(m, "policy_input_hash"); h != "" {
		v.PolicyHash = h
	}
	if a.ApprovalID != "" {
		v.ApprovalID = a.ApprovalID
	}
	if s := metaStr(m, "source_lsn"); s != "" {
		v.SourceLSN = s
	}
	if s := metaStr(m, "applied_lsn"); s != "" {
		v.AppliedLSN = s
	}
	if lag, found := metaInt(m, "cdc_lag_seconds"); found {
		l := lag
		v.LagSeconds = &l
	}
	return v
}

func buildCutoverTimeline(byAction map[string]AuditEntry) map[string]any {
	pre, hasPre := byAction["final_preflight"]
	preV := cutoverStageFromAudit("FINAL_PREFLIGHT", pre, hasPre)

	q, hasQ := byAction["final_quiesce"]
	qV := cutoverStageFromAudit("WRITES_QUIESCED", q, hasQ)

	transfer, hasTransfer := byAction["transfer_ownership"]
	transferOK := hasTransfer && auditSuccess(transfer)
	complete, hasComplete := byAction["cutover_complete"]
	completeOK := hasComplete && auditSuccess(complete)

	// CDC_CATCHING_UP / CDC_CAUGHT_UP / FINAL_VALIDATION are evidenced by a
	// successful ownership transfer (which requires preflight CDC catch-up +
	// reconciliation match) or by cutover completion. Never claim them from
	// rehearsal alone: rehearsal proves the path, not this cutover.
	cdcBase := consoleCutoverStageView{Status: "pending"}
	if transferOK || completeOK {
		src := transfer
		if completeOK {
			src = complete
		}
		m := metaJSON(src.Metadata)
		cdcBase = consoleCutoverStageView{
			Status: "completed", Timestamp: src.CreatedAt,
			RequestID: src.RequestID, Actor: src.ActorID,
			ApprovalID: src.ApprovalID,
			SourceLSN:  metaStr(m, "source_lsn"),
			AppliedLSN: metaStr(m, "applied_lsn"),
		}
		if lag, found := metaInt(m, "cdc_lag_seconds"); found {
			l := lag
			cdcBase.LagSeconds = &l
		}
		if h := metaStr(m, "policy_input_hash"); h != "" {
			cdcBase.PolicyHash = h
		}
	}
	catching := cdcBase
	catching.Name = "CDC_CATCHING_UP"
	catching.Detail = "drain to measured catch-up point"
	caught := cdcBase
	caught.Name = "CDC_CAUGHT_UP"
	if caught.SourceLSN != "" || caught.AppliedLSN != "" {
		caught.Detail = "applied reached pre-probe base"
	}
	validated := cdcBase
	validated.Name = "FINAL_VALIDATION"
	validated.Detail = "reconciliation match"

	ownV := cutoverStageFromAudit("OWNERSHIP_TRANSFERRED", transfer, hasTransfer)

	sw, hasSw := byAction["switch_routing"]
	swV := cutoverStageFromAudit("TRAFFIC_SWITCHED", sw, hasSw)

	rw, hasRw := byAction["resume_writes"]
	rwV := cutoverStageFromAudit("WRITES_RESUMED", rw, hasRw)

	ccV := cutoverStageFromAudit("CUTOVER_COMPLETE", complete, hasComplete)

	stages := []consoleCutoverStageView{preV, qV, catching, caught, validated, ownV, swV, rwV, ccV}
	// Current = first non-completed; blocked takes precedence for display.
	current := "FINAL_PREFLIGHT"
	status := "NOT_STARTED"
	if completeOK {
		current, status = "CUTOVER_COMPLETE", "CUTOVER_COMPLETE"
	} else {
		for _, s := range stages {
			if s.Status != "completed" {
				current = s.Name
				break
			}
		}
		for _, s := range stages {
			if s.Status == "blocked" {
				status = "CUTOVER_BLOCKED"
				break
			}
		}
		if status != "CUTOVER_BLOCKED" {
			if hasPre || hasQ || transferOK {
				status = "CUTTING_OVER"
			}
		}
	}
	out := make([]map[string]any, 0, len(stages))
	for _, s := range stages {
		out = append(out, map[string]any{
			"name": s.Name, "status": s.Status, "timestamp": s.Timestamp,
			"request_id": s.RequestID, "actor": s.Actor,
			"policy_hash": s.PolicyHash, "approval_id": s.ApprovalID,
			"source_lsn": s.SourceLSN, "applied_lsn": s.AppliedLSN,
			"lag_seconds": s.LagSeconds, "detail": s.Detail,
		})
	}
	return map[string]any{"status": status, "current_stage": current, "stages": out}
}

func buildCDCView(wl map[string]any, ownership OwnershipRecord, hasOwnership bool, byAction map[string]AuditEntry) map[string]any {
	rpo := rpoSeconds(wl)
	var sourceLSN, appliedLSN string
	var lag *int64
	var captured, applied, dups *uint64
	var reconMatch *bool
	var reconFP string

	if hasOwnership {
		sourceLSN, appliedLSN = ownership.SourceLSN, ownership.AppliedLSN
		l := ownership.LagSeconds
		lag = &l
	}
	// Latest measured replication evidence (never fabricated history).
	if a, ok := byAction["rehearsal_replication"]; ok {
		m := metaJSON(a.Metadata)
		if sourceLSN == "" {
			sourceLSN = metaStr(m, "source_lsn")
		}
		if appliedLSN == "" {
			appliedLSN = metaStr(m, "applied_lsn")
		}
		if lag == nil {
			if v, found := metaInt(m, "cdc_lag_seconds"); found {
				l := v
				lag = &l
			}
		}
		if v, found := metaUint(m, "events_captured"); found {
			c := v
			captured = &c
		}
		if v, found := metaUint(m, "events_applied"); found {
			c := v
			applied = &c
		}
		if v, found := metaUint(m, "events_duplicates"); found {
			c := v
			dups = &c
		}
	}
	// Transfer/cutover metadata refreshes positions when present.
	for _, act := range []string{"transfer_ownership", "cutover_complete"} {
		if a, ok := byAction[act]; ok && auditSuccess(a) {
			m := metaJSON(a.Metadata)
			if s := metaStr(m, "source_lsn"); s != "" {
				sourceLSN = s
			}
			if s := metaStr(m, "applied_lsn"); s != "" {
				appliedLSN = s
			}
			if v, found := metaInt(m, "cdc_lag_seconds"); found {
				l := v
				lag = &l
			}
		}
	}
	if a, ok := byAction["rehearsal_validation"]; ok {
		m := metaJSON(a.Metadata)
		if raw, has := m["match"]; has {
			if b, isBool := raw.(bool); isBool {
				reconMatch = &b
			}
		}
		reconFP = metaStr(m, "reconciliation_fingerprint")
	}
	var withinRPO *bool
	if lag != nil {
		b := *lag <= rpo
		withinRPO = &b
	}
	return map[string]any{
		"source_lsn": sourceLSN, "applied_lsn": appliedLSN, "lag_seconds": lag,
		"rpo_seconds": rpo, "events_captured": captured,
		"events_applied": applied, "events_duplicates": dups,
		"reconciliation_match": reconMatch,
		"reconciliation_fingerprint": reconFP, "within_rpo": withinRPO,
	}
}

func buildOwnershipView(rec OwnershipRecord, ok bool) map[string]any {
	owner, routing := "aws", "aws"
	prev := ""
	if ok {
		owner, routing, prev = rec.CurrentOwner, rec.Routing, rec.PreviousOwner
	}
	sourceWritable := owner == "aws"
	targetWritable := owner == "azure"
	// Single authoritative record by construction: absent = aws, present =
	// exactly one owner. Dual-authoritative is structurally impossible.
	split := "SAFE"
	return map[string]any{
		"current_owner": owner, "previous_owner": prev, "routing": routing,
		"source_writable": sourceWritable, "target_writable": targetWritable,
		"split_brain": split,
		"source_explanation": map[bool]string{
			true:  "Source (AWS) accepts writes: it is the authoritative provider.",
			false: "Source (AWS) rejects writes with 403 WRITE_NOT_OWNED: it is not authoritative (current authority: " + owner + ").",
		}[sourceWritable],
		"target_explanation": map[bool]string{
			true:  "Target (Azure) accepts writes: it is the authoritative provider.",
			false: "Target (Azure) rejects writes: it is not authoritative (current authority: " + owner + "). Standby replicates via CDC.",
		}[targetWritable],
	}
}

func buildSafetyView(rep CompatReport, hasCompat bool, drift DriftReport, hasDrift bool, byAction map[string]AuditEntry, approvals []Approval, canary []CanaryRecord, ownership OwnershipRecord, hasOwnership bool) map[string]any {
	compat := "UNKNOWN"
	if hasCompat {
		compat = rep.Status
	}
	driftGate, severity := "UNKNOWN", "UNKNOWN"
	if hasDrift {
		driftGate, severity = drift.DriftGate, drift.Severity
	}
	// Latest policy decision across cutover/rehearsal/readiness audits.
	policy := ""
	for _, act := range []string{"final_cutover", "rehearsal_policy", "rehearsal_policy_provisional", "evaluate_cutover_readiness", "final_preflight", "shift_traffic"} {
		if a, ok := byAction[act]; ok && a.PolicyDecision != "" {
			policy = a.PolicyDecision
		}
	}
	if policy == "" {
		for _, a := range byAction {
			if a.PolicyDecision != "" {
				policy = a.PolicyDecision
				break
			}
		}
	}
	if policy == "" {
		policy = "UNKNOWN"
	}
	approval := "NONE"
	if len(approvals) > 0 {
		approval = approvals[len(approvals)-1].ID
	} else {
		// Fall back to any approval referenced by audit (may be pending).
		for _, a := range byAction {
			if a.ApprovalID != "" {
				approval = a.ApprovalID
				break
			}
		}
	}
	canaryVerdict, canaryStage := "UNKNOWN", -1
	if len(canary) > 0 {
		latest := canary[len(canary)-1]
		canaryVerdict, canaryStage = latest.Verdict, latest.Stage
	}
	quiesce := "UNKNOWN"
	if a, ok := byAction["final_quiesce"]; ok && auditSuccess(a) {
		quiesce = "quiesced"
	} else if a, ok := byAction["set_quiesce"]; ok && auditSuccess(a) {
		if strings.Contains(a.Metadata, `"quiesced":true`) {
			quiesce = "quiesced"
		} else if strings.Contains(a.Metadata, `"quiesced":false`) {
			quiesce = "accepting"
		}
	}
	rollback := "PRE_WRITE_AVAILABLE"
	if hasOwnership && ownership.CurrentOwner == "azure" {
		rollback = "POST_WRITE_ROLLBACK_BLOCKED"
	}
	return map[string]any{
		"compatibility": compat, "drift_gate": driftGate, "drift_severity": severity,
		"policy_decision": policy, "approval": approval,
		"canary_verdict": canaryVerdict, "canary_stage": canaryStage,
		"quiesce": quiesce, "rollback": rollback, "split_brain": "SAFE",
	}
}
