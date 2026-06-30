// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package vertex

import "testing"

// ---- parseCountTokensResponse：三种 unwrap 形态 + errors 跳过 + 字符串/数字 totalTokens ----

func TestParseCountTokensResponse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "data.ui.countTokensV2 (number)",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":42}}}}]}]`,
			want: 42,
		},
		{
			name: "data.countTokensV2 (number)",
			raw:  `[{"results":[{"data":{"countTokensV2":{"totalTokens":100}}}]}]`,
			want: 100,
		},
		{
			name: "data.countTokens (number)",
			raw:  `[{"results":[{"data":{"countTokens":{"totalTokens":7}}}]}]`,
			want: 7,
		},
		{
			name: "totalTokens as string",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":"256"}}}}]}]`,
			want: 256,
		},
		{
			name: "single object (not array)",
			raw:  `{"results":[{"data":{"countTokensV2":{"totalTokens":15}}}]}`,
			want: 15,
		},
		{
			name: "entry-level errors skipped",
			raw:  `[{"errors":[{"message":"boom"}]},{"results":[{"data":{"countTokensV2":{"totalTokens":9}}}]}]`,
			want: 9,
		},
		{
			name: "result-level errors skipped",
			raw:  `[{"results":[{"errors":[{"x":1}]},{"data":{"countTokensV2":{"totalTokens":11}}}]}]`,
			want: 11,
		},
		{
			name: "ui preferred over flat",
			raw:  `[{"results":[{"data":{"ui":{"countTokensV2":{"totalTokens":1}},"countTokensV2":{"totalTokens":999}}}]}]`,
			want: 1,
		},
		{
			name: "no countData returns 0",
			raw:  `[{"results":[{"data":{"somethingElse":{}}}]}]`,
			want: 0,
		},
		{
			name: "missing totalTokens returns 0",
			raw:  `[{"results":[{"data":{"countTokensV2":{}}}]}]`,
			want: 0,
		},
		{
			name: "empty results returns 0",
			raw:  `[{"results":[]}]`,
			want: 0,
		},
		{
			name: "invalid json returns 0",
			raw:  `not json{`,
			want: 0,
		},
		{
			name: "json primitive returns 0",
			raw:  `12345`,
			want: 0,
		},
		{
			name: "empty array returns 0",
			raw:  `[]`,
			want: 0,
		},
		{
			name: "totalTokens non-numeric string returns 0",
			raw:  `[{"results":[{"data":{"countTokensV2":{"totalTokens":"abc"}}}]}]`,
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCountTokensResponse(c.raw); got != c.want {
				t.Errorf("parseCountTokensResponse(%s)=%d，期望 %d", c.raw, got, c.want)
			}
		})
	}
}

// ---- coerceTokenCount ----

func TestCoerceTokenCount(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64", float64(42), 42},
		{"float64 truncates", float64(42.9), 42},
		{"int", 7, 7},
		{"numeric string", "123", 123},
		{"trimmed not supported (atoi strict)", " 5 ", 0}, // Atoi 不 trim
		{"non-numeric string", "abc", 0},
		{"nil", nil, 0},
		{"bool", true, 0},
		{"zero float", float64(0), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coerceTokenCount(c.in); got != c.want {
				t.Errorf("coerceTokenCount(%v)=%d，期望 %d", c.in, got, c.want)
			}
		})
	}
}
