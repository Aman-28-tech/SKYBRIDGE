package main

import (
	"reflect"
	"testing"
)

// Parents must drain before children regardless of input order, so an
// order row is never applied before its referenced user (target FK).
func TestOrderTopicsParentsBeforeChildren(t *testing.T) {
	got := orderTopics([]string{"cloudshop.public.orders", "cloudshop.public.users"})
	want := []string{"cloudshop.public.users", "cloudshop.public.orders"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestOrderTopicsFullDependencyChain(t *testing.T) {
	got := orderTopics([]string{
		"cloudshop.public.order_items",
		"cloudshop.public.orders",
		"cloudshop.public.products",
		"cloudshop.public.users",
	})
	want := []string{
		"cloudshop.public.users",
		"cloudshop.public.products",
		"cloudshop.public.orders",
		"cloudshop.public.order_items",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestOrderTopicsDropsEmptiesKeepsUnknown(t *testing.T) {
	got := orderTopics([]string{"", "  ", "cloudshop.public.orders", "custom.topic"})
	want := []string{"cloudshop.public.orders", "custom.topic"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
