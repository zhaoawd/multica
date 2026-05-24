package service

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// Golden routing matrix test (§14.3).
//
// This test is the contract for service.RouteLarkEvent. It loads the
// declared cases from testdata/routing_matrix.golden.yaml, runs each
// through the decision function, and asserts both the positive
// expectations (Channels, Card) and the negative one (MustNotChannels).
//
// Why so verbose: §6.1's routing table is the integration's command
// surface. A regression that flips one cell silently affects which
// chat a user's signal lands in — there is no production observable
// that catches "issue went to the wrong channel" without after-the-
// fact human report. Pinning the matrix as a golden makes that bug
// shape impossible to land without an explicit diff.

type routingCase struct {
	Name       string                `yaml:"name"`
	Conditions LarkRoutingConditions `yaml:"conditions"`
	Expected   routingExpected       `yaml:"expected"`
}

type routingExpected struct {
	Channels        []LarkChannel `yaml:"channels"`
	Card            LarkCardKind  `yaml:"card,omitempty"`
	MustNotChannels []LarkChannel `yaml:"must_not_channels"`
}

type routingMatrixFile struct {
	Cases []routingCase `yaml:"cases"`
}

func loadRoutingMatrix(t *testing.T) routingMatrixFile {
	t.Helper()
	path := filepath.Join("testdata", "routing_matrix.golden.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var matrix routingMatrixFile
	if err := yaml.Unmarshal(data, &matrix); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(matrix.Cases) == 0 {
		t.Fatalf("%s: no cases parsed", path)
	}
	return matrix
}

func TestRouteLarkEvent_Golden(t *testing.T) {
	matrix := loadRoutingMatrix(t)

	for _, c := range matrix.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := RouteLarkEvent(c.Conditions)

			if !equalChannelSlices(got.Channels, c.Expected.Channels) {
				t.Errorf("channels mismatch:\n  want: %s\n  got:  %s",
					channelsStr(c.Expected.Channels), channelsStr(got.Channels))
			}
			if got.Card != c.Expected.Card {
				t.Errorf("card mismatch: want %q, got %q", c.Expected.Card, got.Card)
			}
			if !equalChannelSlices(got.MustNotChannels, c.Expected.MustNotChannels) {
				t.Errorf("must_not_channels mismatch:\n  want: %s\n  got:  %s",
					channelsStr(c.Expected.MustNotChannels), channelsStr(got.MustNotChannels))
			}

			// Cross-check: every channel the router picked must NOT
			// appear in MustNotChannels, and the complement must be
			// total (covers allChannels). A bug in canonicalisation
			// would land here before it lands in production.
			if overlap := intersect(got.Channels, got.MustNotChannels); len(overlap) > 0 {
				t.Errorf("channels ∩ must_not_channels nonempty: %v", overlap)
			}
			if total := len(got.Channels) + len(got.MustNotChannels); total != len(allChannels) {
				t.Errorf("channels ∪ must_not_channels must cover allChannels (%d), got %d",
					len(allChannels), total)
			}
		})
	}
}

// TestRouteLarkEvent_CoverageMatchesSupportedEvents enforces the §14.3
// rule that "every row in SupportedLarkEvents must have at least one
// case in the matrix". The mirror of this rule is "no case references
// an event outside SupportedLarkEvents" — which would otherwise
// silently land routing for events the rest of the system doesn't
// know are routable. Both directions are checked here.
func TestRouteLarkEvent_CoverageMatchesSupportedEvents(t *testing.T) {
	matrix := loadRoutingMatrix(t)

	covered := make(map[string]bool)
	for _, c := range matrix.Cases {
		covered[c.Conditions.Event] = true
	}
	supported := make(map[string]bool, len(SupportedLarkEvents))
	for _, e := range SupportedLarkEvents {
		supported[e] = true
	}

	var missing []string
	for _, e := range SupportedLarkEvents {
		if !covered[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("supported events without any golden case: %v", missing)
	}

	var stray []string
	for ev := range covered {
		if !supported[ev] {
			stray = append(stray, ev)
		}
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		t.Errorf("golden case references events outside SupportedLarkEvents: %v", stray)
	}
}

// TestRouteLarkEvent_UnsupportedEventIsSilent guards the router's
// fall-through: an event not in SupportedLarkEvents must return a
// silent decision (no channels, no card). The dispatcher relies on
// this to short-circuit before any chat / DM I/O when a future
// protocol change adds an event the router doesn't yet handle.
func TestRouteLarkEvent_UnsupportedEventIsSilent(t *testing.T) {
	got := RouteLarkEvent(LarkRoutingConditions{
		Event:       "definitely:not:a:lark:event",
		HasAssignee: true,
	})
	if len(got.Channels) != 0 {
		t.Errorf("unknown event must produce 0 channels, got %v", got.Channels)
	}
	if got.Card != LarkCardNone {
		t.Errorf("unknown event must produce empty card, got %q", got.Card)
	}
	// Negative space is total: every channel is off-limits.
	if len(got.MustNotChannels) != len(allChannels) {
		t.Errorf("unknown event must have full MustNotChannels (%d), got %d",
			len(allChannels), len(got.MustNotChannels))
	}
}

func TestDefaultLarkUserPref_OnlyAssignedAndClarification(t *testing.T) {
	// Locks the §6.1 default: OAuth-bound users see Assigned +
	// AgentClarification on, everything else off. A change to this
	// default ships only with a deliberate test update.
	pref := DefaultLarkUserPref()
	if !pref.AssignedDM {
		t.Error("AssignedDM should default ON")
	}
	if !pref.AgentClarificationDM {
		t.Error("AgentClarificationDM should default ON")
	}
	if pref.TaskFailedDM || pref.TaskCompletedDM || pref.MentionDM {
		t.Errorf("only Assigned + AgentClarification should default ON, got %+v", pref)
	}
}

func TestIsLarkRoutableEvent(t *testing.T) {
	for _, e := range SupportedLarkEvents {
		if !IsLarkRoutableEvent(e) {
			t.Errorf("SupportedLarkEvents includes %q but IsLarkRoutableEvent rejects it", e)
		}
	}
	for _, e := range []string{"", "issue:archived", "comment:resolved"} {
		if IsLarkRoutableEvent(e) {
			t.Errorf("unsupported event %q should not be routable", e)
		}
	}
}

// equalChannelSlices compares two channel slices treating empty and
// nil as equal. Order is significant — both router output and golden
// expectations are normalised to canonical order, so a mismatch here
// means an actual routing-set difference, not a sort issue.
func equalChannelSlices(a, b []LarkChannel) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func intersect(a, b []LarkChannel) []LarkChannel {
	in := make(map[LarkChannel]struct{}, len(a))
	for _, c := range a {
		in[c] = struct{}{}
	}
	var out []LarkChannel
	for _, c := range b {
		if _, ok := in[c]; ok {
			out = append(out, c)
		}
	}
	return out
}

func channelsStr(cs []LarkChannel) string {
	if len(cs) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, string(c))
	}
	return fmt.Sprintf("[%s]", joinStrings(out, ", "))
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}
