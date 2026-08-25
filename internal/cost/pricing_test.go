package cost

import (
	"sort"
	"testing"
)

func TestLoadPricing(t *testing.T) {
	db, err := LoadPricing()
	if err != nil {
		t.Fatalf("LoadPricing: %v", err)
	}
	if db.Meta.DefaultRegion == "" {
		t.Error("expected a default region in embedded pricing metadata")
	}
	if len(db.Regions) == 0 {
		t.Fatal("expected at least one region in embedded pricing data")
	}
	if _, ok := db.Regions[db.Meta.DefaultRegion]; !ok {
		t.Errorf("default region %q has no pricing entry", db.Meta.DefaultRegion)
	}
}

func TestForRegion(t *testing.T) {
	db, err := LoadPricing()
	if err != nil {
		t.Fatalf("LoadPricing: %v", err)
	}

	t.Run("known region", func(t *testing.T) {
		p, err := db.ForRegion("us-east-1")
		if err != nil {
			t.Fatalf("ForRegion: %v", err)
		}
		if p.Lambda.RequestPerMillion <= 0 {
			t.Error("expected positive Lambda request price for us-east-1")
		}
	})

	t.Run("empty region falls back to default", func(t *testing.T) {
		p, err := db.ForRegion("")
		if err != nil {
			t.Fatalf("ForRegion(\"\"): %v", err)
		}
		want := db.Regions[db.Meta.DefaultRegion]
		if p != want {
			t.Errorf("ForRegion(\"\") = %p, want default region's pricing %p", p, want)
		}
	})

	t.Run("unknown region errors", func(t *testing.T) {
		_, err := db.ForRegion("mars-central-1")
		if err == nil {
			t.Fatal("expected error for unknown region, got nil")
		}
	})
}

func TestRegionList(t *testing.T) {
	db, err := LoadPricing()
	if err != nil {
		t.Fatalf("LoadPricing: %v", err)
	}
	got := db.RegionList()
	if len(got) != len(db.Regions) {
		t.Fatalf("RegionList returned %d regions, want %d", len(got), len(db.Regions))
	}
	sort.Strings(got)
	for _, r := range got {
		if _, ok := db.Regions[r]; !ok {
			t.Errorf("RegionList returned %q, not present in db.Regions", r)
		}
	}
}
