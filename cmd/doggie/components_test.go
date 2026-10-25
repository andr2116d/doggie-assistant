package main

import (
	"context"
	"slices"
	"testing"

	"github.com/andr2116d/doggie-assistant/internal/supervisor"
)

type stub string

func (s stub) Name() string                  { return string(s) }
func (s stub) Run(ctx context.Context) error { <-ctx.Done(); return nil }

func TestSelectComponents(t *testing.T) {
	all := []supervisor.Component{stub("a"), stub("b"), stub("c")}
	tests := []struct {
		name         string
		only, except string
		want         []string
		wantErr      bool
	}{
		{name: "no filter", want: []string{"a", "b", "c"}},
		{name: "only", only: "a,c", want: []string{"a", "c"}},
		{name: "only with spaces", only: " b ", want: []string{"b"}},
		{name: "except", except: "b", want: []string{"a", "c"}},
		{name: "except all", except: "a,b,c", wantErr: true},
		{name: "unknown", only: "x", wantErr: true},
		{name: "both flags", only: "a", except: "b", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectComponents(all, tt.only, tt.except)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", names(got))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(names(got), tt.want) {
				t.Errorf("got %v, want %v", names(got), tt.want)
			}
		})
	}
}
