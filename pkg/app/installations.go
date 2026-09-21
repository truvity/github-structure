package app

// Resolving a ruleset's bypass App names. A ruleset names an App; the
// REST API takes the App's DATABASE id as an Integration bypass actor;
// the organization's installation list is where one becomes the other.
//
// The registry deliberately does not carry that id. It is GitHub's fact
// about an App, and a copy of it in a file is stale the moment the App
// is recreated — silently, because a bypass actor that does not resolve
// says nothing until the release act it permits is refused.

import (
	"context"
	"fmt"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// installation is one App installed on the organization, cut down to the
// only question this package asks of it: which App is this, and what is
// the id a ruleset's bypass actor has to carry? The installation's own
// id, its repository scope and its permissions are the credential
// custodian's business, not this library's.
type installation struct { //nolint:grouper // one type in this file; a group of one reads worse
	AppID   int64  `json:"app_id"`
	AppSlug string `json:"app_slug"`
}

// installationsPath is the one endpoint that answers "which Apps are
// installed on this organization, and what are their database ids". Both
// auth paths in this package ask it — the engine App at deploy, the
// operator's CLI at drift — and they must ask the same question, or a
// deploy and a drift report could disagree about which actor a ruleset
// names.
func installationsPath(org string) string {
	return fmt.Sprintf("/orgs/%s/installations?per_page=100", org)
}

// InstalledAppIDs reads the organization's App installations through the
// App client — the DEPLOY path, where there is no gh CLI and no operator
// token, only the engine App's own installation token.
//
// Needs organization_administration: read on the engine App. It is asked
// for only where a ruleset actually names a bypass App, so an estate
// whose rules name none never needs the permission.
func InstalledAppIDs(ctx context.Context, client *Client, org string) (registry.InstalledApps, error) {
	var page struct {
		Installations []installation `json:"installations"`
	}

	if err := client.Get(ctx, installationsPath(org), &page); err != nil {
		return nil, fmt.Errorf("list app installations on %s: %w", org, err)
	}

	return appIDsBySlug(page.Installations), nil
}

// installedAppIDs is the same answer read with the operator's own gh
// credentials — the DRIFT path, where there is no App key to hand.
func installedAppIDs(ctx context.Context, org string) (registry.InstalledApps, error) {
	var page struct {
		Installations []installation `json:"installations"`
	}

	if err := ghAPI(ctx, installationsPath(org), &page); err != nil {
		return nil, fmt.Errorf("list app installations on %s: %w", org, err)
	}

	return appIDsBySlug(page.Installations), nil
}

// appIDsBySlug keys installations the way a ruleset names them: by the
// App's slug, which is what a human reads in the App's URL.
func appIDsBySlug(live []installation) registry.InstalledApps {
	apps := make(registry.InstalledApps, len(live))
	for _, inst := range live {
		apps[inst.AppSlug] = int(inst.AppID)
	}

	return apps
}
