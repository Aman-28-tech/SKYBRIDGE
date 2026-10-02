// Redpanda topic mapping.
//
// Decision: retain the Debezium default topic naming
// (<topic.prefix>.<schema>.<table>) rather than introducing a second
// SKYBRIDGE-specific scheme.
//
// Why:
//   - The connector already emits topic.prefix=cloudshop; renaming would add
//     a Single Message Transform + mapping table with no correctness benefit.
//   - Default names are stable, per-table (ordering per table is preserved),
//     and directly traceable to source tables in runbooks.
//   - A skybridge.<workload>.cdc single-topic design would merge tables,
//     complicate per-table ordering reasoning, and proliferate nothing but
//     confusion. Deferred until a real fan-in requirement exists.
//
// Convention (authoritative business tables only):
//   cloudshop.public.users
//   cloudshop.public.products
//   cloudshop.public.orders
//   cloudshop.public.order_items
//
// Explicitly NOT captured: public.idempotency_keys, public.applied_jobs,
// public.schema_version, public.cdc_* (infrastructure, churn-heavy or
// applier-owned). Source filtering is enforced in the connector config
// (table.include.list) and re-validated by TopicForTable + tests.
package cdc

import (
	"fmt"
	"strings"
)

const topicPrefix = "cloudshop"
const topicSchema = "public"

// BusinessTopics is the exact set the connector must capture.
var BusinessTopics = []string{
	"cloudshop.public.users",
	"cloudshop.public.products",
	"cloudshop.public.orders",
	"cloudshop.public.order_items",
}

// ExcludedTables documents infrastructure never replicated as business state.
var ExcludedTables = []string{
	"idempotency_keys", "applied_jobs", "schema_version",
	"cdc_applied_events", "cdc_applied_txn", "cdc_offsets",
}

// TopicForTable maps a business table to its Redpanda topic.
func TopicForTable(table string) (string, error) {
	if !allowedTables[table] {
		return "", fmt.Errorf("table %q is not a replicated business table", table)
	}
	return topicPrefix + "." + topicSchema + "." + table, nil
}

// TableForTopic is the inverse mapping; errors on unknown topics.
func TableForTopic(topic string) (string, error) {
	parts := strings.Split(topic, ".")
	if len(parts) != 3 || parts[0] != topicPrefix || parts[1] != topicSchema {
		return "", fmt.Errorf("unknown topic %q", topic)
	}
	if !allowedTables[parts[2]] {
		return "", fmt.Errorf("topic %q is not a replicated business table", topic)
	}
	return parts[2], nil
}

// ConnectorTableList renders the Debezium table.include.list value.
func ConnectorTableList() string {
	return "public.users,public.products,public.orders,public.order_items"
}
