package registry_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	registry "github.com/truvity/github-structure/pkg/registry"
)

// withVars puts a variables block on the minimal registry's repository row.
func withVars(t *testing.T, block string) (*registry.Config, error) {
	t.Helper()

	return load(t, strings.Replace(minimal, "        access: [managers]\n", "        access: [managers]\n"+block, 1))
}

func TestVariablesLoad(t *testing.T) {
	cfg, err := withVars(t, `        variables:
          RELEASE_ROLE_ARN: arn:test:iam::1234:role/release-widget
          DEPLOY_REGION: eu-central-1
          AWS_KMS_KEY_ARN: arn:test:kms:eu-central-1:1234:key/1234abcd
`)
	require.NoError(t, err)

	repo := cfg.Orgs["acme"].Repos["widget"]
	assert.Equal(t, []string{"AWS_KMS_KEY_ARN", "DEPLOY_REGION", "RELEASE_ROLE_ARN"}, repo.SortedVariables())
	assert.Equal(t, "arn:test:iam::1234:role/release-widget", repo.Variables["RELEASE_ROLE_ARN"])
}

func TestVariablesRefusals(t *testing.T) {
	cases := map[string]struct {
		block string
		want  string
	}{
		"name with a dash": {
			"        variables:\n          MY-VAR: x\n",
			"the name may hold letters, digits and underscore",
		},
		"name starting with a digit": {
			"        variables:\n          1VAR: x\n",
			"must not start with a digit",
		},
		"GitHub's own prefix": {
			"        variables:\n          GITHUB_THING: x\n",
			"are GitHub's own",
		},
		"empty value": {
			"        variables:\n          REGION: \"\"\n",
			"the value is empty",
		},
		"names differing by case": {
			"        variables:\n          REGION: a\n          region: b\n",
			"differ only by case",
		},
		"a secret name": {
			"        variables:\n          DEPLOY_SECRET: hunter2\n",
			"reads as a secret",
		},
		"a token name": {
			"        variables:\n          npm_token: abc\n",
			"reads as a secret",
		},
		"a password name": {
			"        variables:\n          DB_PASSWORD: abc\n",
			"reads as a secret",
		},
		"a key name with a plain value": {
			"        variables:\n          SIGNING_KEY: abc\n",
			"reads as a secret",
		},
		"a key name with a value that only starts like an ARN": {
			"        variables:\n          SIGNING_KEY: arn-of-the-key\n",
			"reads as a secret",
		},
		"a secret name with a value that is not an ARN": {
			"        variables:\n          SECRET_ID: secretsmanager:prod/db\n",
			"reads as a secret",
		},
		"an unknown field of the row": {
			"        variable:\n          A: b\n",
			"field variable not found",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := withVars(t, tc.block)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// An ARN names a thing and does not open it, so a name that reads as a
// secret may carry one.
func TestVariablesAllowAnARNUnderASecretLookingName(t *testing.T) {
	for _, arn := range []string{
		"arn:test:iam::1234:role/release-widget",
		"arn:test:secretsmanager:eu-central-1:1234:secret:prod/db-AbCdEf",
		"arn:test-gov:kms:gov-west-1:1234:key/abc",
	} {
		_, err := withVars(t, "        variables:\n          SECRET_ARN: "+arn+"\n")
		require.NoError(t, err, arn)
	}
}

func TestVariablesOnAnArchivedRepository(t *testing.T) {
	_, err := withVars(t, "        archived: true\n        variables:\n          REGION: eu\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived, so variables do nothing")
}
