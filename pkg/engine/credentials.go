package engine

import (
	"context"
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/github-structure/pkg/registry"
)

type (
	// KV is one opened KV secret store: the reader of an org whose engine
	// App credentials are pushed to a KV secret.
	KV interface {
		// Read returns the secret's properties, or nil when nothing is
		// there yet.
		Read(ctx context.Context, path string) (map[string]string, error)
		// Revoke drops the session the store was opened with.
		Revoke(ctx context.Context)
	}

	// CredentialSources is how an estate reaches the places an org's
	// engine App credentials can live. The registry says WHICH place an
	// org uses; this says how to read it.
	CredentialSources struct {
		// OpenKV opens the KV mount an org's `openbao` credential names.
		// Required when any org uses one.
		OpenKV func(ctx context.Context, secret *registry.OpenBAOSecret) (KV, error)
		// ReadParameter reads one mirrored parameter by its full name.
		// Required when any org uses an `ssm_prefix` credential.
		ReadParameter func(c *pulumi.Context, name string) (string, error)
		// Properties are the three property names of a pushed KV secret.
		Properties KVProperties
		// NothingThereHint is appended to the refusal for a KV secret that
		// has not been pushed yet: what the operator must do first.
		NothingThereHint string
	}

	// KVProperties are the property names of a pushed engine App secret.
	KVProperties struct {
		AppID          string
		InstallationID string
		PrivateKey     string
	}
)

// LoadCredentials reads the three values the engine acts as for one org.
// The registry guarantees exactly one source, so the choice here is a
// fact of the row and never a fallback: a store that is down or a key
// that has not been pushed yet must STOP the run, naming the org and the
// place, rather than quietly reaching for the other store and applying as
// the wrong App.
func LoadCredentials(c *pulumi.Context, org string, source *registry.EngineCredentials, from CredentialSources) (Credentials, error) {
	if source == nil {
		return Credentials{}, fmt.Errorf("org %q declares no engine_credentials", org)
	}

	if source.OpenBAO != nil {
		return kvCredentials(c.Context(), org, source, from)
	}

	return parameterCredentials(c, org, source, from)
}

func kvCredentials(ctx context.Context, org string, source *registry.EngineCredentials, from CredentialSources) (Credentials, error) {
	if from.OpenKV == nil {
		return Credentials{}, fmt.Errorf("org %q engine credentials (%s): this estate gave no way to open a KV store", org, source.Describe())
	}

	secret := source.OpenBAO

	kv, err := from.OpenKV(ctx, secret)
	if err != nil {
		return Credentials{}, fmt.Errorf("org %q engine credentials (%s): %w", org, source.Describe(), err)
	}
	defer kv.Revoke(ctx)

	data, err := kv.Read(ctx, secret.Path)
	if err != nil {
		return Credentials{}, fmt.Errorf("org %q engine credentials (%s): %w", org, source.Describe(), err)
	}

	return CredentialsFromKV(org, source, data, from.Properties, from.NothingThereHint)
}

// CredentialsFromKV maps one pushed KV secret onto the three values the
// engine needs, and refuses anything else by naming the org, the place
// and the property: a run that guessed here would authenticate as nobody,
// or as half an App, and say so only once GitHub answered 401.
func CredentialsFromKV(org string, source *registry.EngineCredentials, data map[string]string, props KVProperties, hint string) (Credentials, error) {
	if data == nil {
		return Credentials{}, fmt.Errorf("org %q engine credentials (%s): nothing there. %s", org, source.Describe(), hint)
	}

	creds := Credentials{}
	want := []string{props.AppID, props.InstallationID, props.PrivateKey}

	for _, f := range []struct {
		property string
		dst      *string
	}{
		{props.AppID, &creds.AppID},
		{props.InstallationID, &creds.InstallationID},
		{props.PrivateKey, &creds.PrivateKey},
	} {
		value := data[f.property]
		if value == "" {
			return Credentials{}, fmt.Errorf(
				"org %q engine credentials (%s): property %q is missing or empty, want %v. A half-written secret is a "+
					"push that has not finished, or an App whose key was regenerated in GitHub's own settings",
				org, source.Describe(), f.property, want)
		}

		*f.dst = value
	}

	return creds, nil
}

// parameterCredentials reads the three mirrored parameters under one
// prefix: a key-held App whose item is mirrored into a parameter store.
func parameterCredentials(c *pulumi.Context, org string, source *registry.EngineCredentials, from CredentialSources) (Credentials, error) {
	if from.ReadParameter == nil {
		return Credentials{}, fmt.Errorf("org %q engine credentials (%s): this estate gave no way to read a parameter", org, source.Describe())
	}

	creds := Credentials{}

	for _, f := range []struct {
		field string
		dst   *string
	}{
		{registry.FieldAppID, &creds.AppID},
		{registry.FieldInstallationID, &creds.InstallationID},
		{registry.FieldPrivateKey, &creds.PrivateKey},
	} {
		value, err := from.ReadParameter(c, source.SSMPrefix+"/"+f.field)
		if err != nil {
			return Credentials{}, fmt.Errorf("org %q engine credentials (%s): %w", org, source.Describe(), err)
		}

		*f.dst = value
	}

	return creds, nil
}
