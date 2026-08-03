// channel_id_test.go — coverage for the #link-feed channel-id resolution.
//
// The channel id used to be a compiled-in constant naming one operator's
// private Discord channel. It now comes from the untracked auth.yaml or from
// an environment variable. The failure this guards is a quiet one: if the
// resolver ever fell back to some default instead of erroring, the puller
// would fetch from the wrong channel rather than telling the operator that
// configuration is missing.
package linkfeed

import (
	"strings"
	"testing"
)

func TestResolveChannelID_PrefersAuthYAML(t *testing.T) {
	t.Setenv(LinkFeedChannelEnvVar, "222222222222222222")

	got, err := resolveChannelID(&discordAuth{Token: "t", ChannelID: "111111111111111111"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "111111111111111111" {
		t.Errorf("resolveChannelID() = %q; want the auth.yaml value to win over the env var", got)
	}
}

func TestResolveChannelID_FallsBackToEnv(t *testing.T) {
	t.Setenv(LinkFeedChannelEnvVar, "222222222222222222")

	got, err := resolveChannelID(&discordAuth{Token: "t"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "222222222222222222" {
		t.Errorf("resolveChannelID() = %q; want the env var", got)
	}
}

func TestResolveChannelID_TrimsEnvWhitespace(t *testing.T) {
	t.Setenv(LinkFeedChannelEnvVar, "  222222222222222222\n")

	got, err := resolveChannelID(&discordAuth{Token: "t"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "222222222222222222" {
		t.Errorf("resolveChannelID() = %q; want whitespace trimmed", got)
	}
}

// TestResolveChannelID_ErrorsWhenUnset is the important one: unconfigured must
// be an error, never a silent default that pulls from someone else's channel.
func TestResolveChannelID_ErrorsWhenUnset(t *testing.T) {
	t.Setenv(LinkFeedChannelEnvVar, "")

	for _, auth := range []*discordAuth{nil, {Token: "t"}, {Token: "t", ChannelID: ""}} {
		got, err := resolveChannelID(auth)
		if err == nil {
			t.Fatalf("resolveChannelID(%+v) = %q, nil; want an error when unconfigured", auth, got)
		}
		if got != "" {
			t.Errorf("resolveChannelID returned %q alongside an error; want empty", got)
		}
		// The message must name both places the operator can set it.
		for _, want := range []string{discordAuthRelPath, LinkFeedChannelEnvVar} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	}
}
