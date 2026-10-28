package store

import (
	"math"
	"strings"
	"testing"
)

func TestVectorIndexSourceChecksumCanonicalFormat(t *testing.T) {
	t.Parallel()
	// Goldens hash the literal domain "docbank-vector-index-source/v1\x00"
	// followed by the hand-encoded member bytes documented in each case.
	tests := []struct {
		name    string
		members []VectorIndexMember
		want    string
	}{
		{
			name: "empty",
			want: "762c78b52729ec285e4197a1d5725cac02c3e58a8cf2dc8090c1d6205e80f15f",
		},
		{
			// 0:0:0:1:0
			name:    "empty fields",
			members: []VectorIndexMember{{}},
			want:    "dbdfe5dc97c597f63a1fc446460b4ba02315e93328e916e0218804f08cc64e37",
		},
		{
			// 1:e1:v1:h2:-7
			name: "field order and negative size",
			members: []VectorIndexMember{{
				EmbeddingSetID: "e", VectorSetID: "v", PayloadBlobHash: "h", PayloadSize: -7,
			}},
			want: "41e2ba715712c8b178254d5238f2020a916dbb290412e6775542fa1bc952486e",
		},
		{
			// "3:猫2:\x00x3:\xff:\x803:-12" counts bytes, including invalid UTF-8.
			name: "unicode NUL and invalid UTF8 bytes",
			members: []VectorIndexMember{{
				EmbeddingSetID: "猫", VectorSetID: "\x00x", PayloadBlobHash: "\xff:\x80", PayloadSize: -12,
			}},
			want: "8e8e33a71fdf0183e11efd58ff7891affb9d0000f3f454784803fbe48cc60cc8",
		},
		{
			// 1:a1:b1:c20:-92233720368547758081:d1:e1:f19:9223372036854775807
			name: "signed int64 bounds",
			members: []VectorIndexMember{
				{EmbeddingSetID: "a", VectorSetID: "b", PayloadBlobHash: "c", PayloadSize: math.MinInt64},
				{EmbeddingSetID: "d", VectorSetID: "e", PayloadBlobHash: "f", PayloadSize: math.MaxInt64},
			},
			want: "652f878a1d4207d977bb9e94373af88ef68bd5b3136b0443ae2596ecd5df267e",
		},
		{
			// 1:a1:b1:c1:11:d1:e1:f1:2
			name: "member order",
			members: []VectorIndexMember{
				{EmbeddingSetID: "a", VectorSetID: "b", PayloadBlobHash: "c", PayloadSize: 1},
				{EmbeddingSetID: "d", VectorSetID: "e", PayloadBlobHash: "f", PayloadSize: 2},
			},
			want: "20605f2415ef2efe04679acd6536611974cb2bb2d53bc39f3e18d8122345997c",
		},
		{
			// 1:d1:e1:f1:21:a1:b1:c1:1
			name: "reversed member order",
			members: []VectorIndexMember{
				{EmbeddingSetID: "d", VectorSetID: "e", PayloadBlobHash: "f", PayloadSize: 2},
				{EmbeddingSetID: "a", VectorSetID: "b", PayloadBlobHash: "c", PayloadSize: 1},
			},
			want: "2f0d817c5151b9d188beafd1314f8135307e40c03ea50ee34fa74e716b2da8be",
		},
		{
			// 2:ab1:c0:2:101:a2:bc0:2:10
			name: "length prefixes distinguish adjacent fields",
			members: []VectorIndexMember{
				{EmbeddingSetID: "ab", VectorSetID: "c", PayloadSize: 10},
				{EmbeddingSetID: "a", VectorSetID: "bc", PayloadSize: 10},
			},
			want: "094142592199150468766510b3a0693375fa3936f84cfa6477242df502fc9450",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vectorIndexSourceChecksum(tt.members); got != tt.want {
				t.Fatalf("source checksum = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestVectorIndexSourceChecksumAllocationGrowth(t *testing.T) { //nolint:paralleltest // AllocsPerRun panics during parallel tests
	// Materialize inputs before measuring so only serialization is counted.
	members := make([]VectorIndexMember, 128)
	for i := range members {
		members[i] = VectorIndexMember{
			EmbeddingSetID: strings.Repeat("a", 64), VectorSetID: strings.Repeat("b", 64),
			PayloadBlobHash: strings.Repeat("c", 64), PayloadSize: math.MaxInt64,
		}
	}
	var checksum string
	small := testing.AllocsPerRun(20, func() {
		checksum = vectorIndexSourceChecksum(members[:1])
	})
	large := testing.AllocsPerRun(20, func() {
		checksum = vectorIndexSourceChecksum(members)
	})
	if checksum == "" {
		t.Fatal("source checksum is empty")
	}
	t.Logf("source checksum allocations: 1 member %.0f, 128 members %.0f", small, large)
	if large > small+16 {
		t.Fatalf("source checksum allocations grow with membership: 1 member %.0f, 128 members %.0f; want large <= small + 16", small, large)
	}
}
