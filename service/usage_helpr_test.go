package service

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

func TestHasUpstreamTokenUsage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		usage *dto.Usage
		want  bool
	}{
		{name: "nil", usage: nil, want: false},
		{name: "all zeros", usage: &dto.Usage{}, want: false},
		{name: "prompt only", usage: &dto.Usage{PromptTokens: 10}, want: true},
		{name: "completion only", usage: &dto.Usage{CompletionTokens: 4}, want: true},
		{name: "input tokens only", usage: &dto.Usage{InputTokens: 8}, want: true},
		{name: "output tokens only", usage: &dto.Usage{OutputTokens: 3}, want: true},
		{
			name: "cache read only",
			usage: &dto.Usage{
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 12},
			},
			want: true,
		},
		{
			name: "cache creation only",
			usage: &dto.Usage{
				PromptTokensDetails: dto.InputTokenDetails{CachedCreationTokens: 7},
			},
			want: true,
		},
		{
			name:  "claude 5m cache only",
			usage: &dto.Usage{ClaudeCacheCreation5mTokens: 5},
			want:  true,
		},
		{
			name:  "claude 1h cache only",
			usage: &dto.Usage{ClaudeCacheCreation1hTokens: 2},
			want:  true,
		},
		{
			name: "input nonzero output zero",
			usage: &dto.Usage{
				PromptTokens:     100,
				CompletionTokens: 0,
				TotalTokens:      100,
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, HasUpstreamTokenUsage(tc.usage))
		})
	}
}

func TestValidUsageUnchanged(t *testing.T) {
	t.Parallel()

	require.False(t, ValidUsage(nil))
	require.False(t, ValidUsage(&dto.Usage{}))
	require.True(t, ValidUsage(&dto.Usage{PromptTokens: 1}))
	require.True(t, ValidUsage(&dto.Usage{CompletionTokens: 1}))
	require.False(t, ValidUsage(&dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 9},
	}))
}
