package zka

import "testing"

func TestCredentialProviderReconnectTargetsFollowNodeOwnedBindings(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.state.Node.ID = "provider"
	d.state.Workspaces["owned"] = &Workspace{
		ID: "owned", RemoteHost: "devbox",
		CredentialClaim: &CredentialClaim{ProviderSource: "remote", OwnerNodeID: "provider"},
	}
	d.state.Workspaces["other"] = &Workspace{
		ID: "other", RemoteHost: "otherbox",
		CredentialClaim: &CredentialClaim{ProviderSource: "remote", OwnerNodeID: "someone-else"},
	}
	d.state.Workspaces["local"] = &Workspace{
		ID: "local", CredentialClaim: &CredentialClaim{ProviderSource: "local", OwnerNodeID: "provider"},
	}
	d.state.Remotes["cachedbox"] = &RemoteCache{Host: "cachedbox", Workspaces: map[string]*Workspace{
		"cached": {ID: "cached", CredentialClaim: &CredentialClaim{ProviderSource: "remote", OwnerNodeID: "provider"}},
	}}
	d.mu.Unlock()

	targets := d.credentialProviderReconnectTargets()
	if len(targets) != 2 || targets["devbox"] != "owned" || targets["cachedbox"] != "cached" {
		t.Fatalf("provider reconnect targets = %#v", targets)
	}
}
