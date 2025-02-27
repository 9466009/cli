package set

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/api"
	"github.com/cli/cli/internal/config"
	"github.com/cli/cli/internal/ghrepo"
	"github.com/cli/cli/pkg/cmd/secret/shared"
	"github.com/cli/cli/pkg/cmdutil"
	"github.com/cli/cli/pkg/iostreams"
	"github.com/cli/cli/pkg/prompt"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/nacl/box"
)

type SetOptions struct {
	HttpClient func() (*http.Client, error)
	IO         *iostreams.IOStreams
	Config     func() (config.Config, error)
	BaseRepo   func() (ghrepo.Interface, error)

	RandomOverride io.Reader

	SecretName      string
	OrgName         string
	EnvName         string
	Body            string
	Visibility      string
	RepositoryNames []string
}

func NewCmdSet(f *cmdutil.Factory, runF func(*SetOptions) error) *cobra.Command {
	opts := &SetOptions{
		IO:         f.IOStreams,
		Config:     f.Config,
		HttpClient: f.HttpClient,
	}

	cmd := &cobra.Command{
		Use:   "set <secret-name>",
		Short: "Create or update secrets",
		Long:  "Locally encrypt a new or updated secret at either the repository, environment, or organization level and send it to GitHub for storage.",
		Example: heredoc.Doc(`
			Paste secret in prompt
			$ gh secret set MYSECRET

			Use environment variable as secret value
			$ gh secret set MYSECRET  -b"${ENV_VALUE}"

			Use file as secret value
			$ gh secret set MYSECRET < file.json

			Set environment level secret
			$ gh secret set MYSECRET -bval --env=anEnv

			Set organization level secret visible to entire organization
			$ gh secret set MYSECRET -bval --org=anOrg --visibility=all

			Set organization level secret visible only to certain repositories
			$ gh secret set MYSECRET -bval --org=anOrg --repos="repo1,repo2,repo3"
`),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return &cmdutil.FlagError{Err: errors.New("must pass single secret name")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// support `-R, --repo` override
			opts.BaseRepo = f.BaseRepo

			if err := cmdutil.MutuallyExclusive("specify only one of `--org` or `--env`", opts.OrgName != "", opts.EnvName != ""); err != nil {
				return err
			}

			opts.SecretName = args[0]

			err := validSecretName(opts.SecretName)
			if err != nil {
				return err
			}

			if cmd.Flags().Changed("visibility") {
				if opts.OrgName == "" {
					return &cmdutil.FlagError{Err: errors.New(
						"--visibility not supported for repository secrets; did you mean to pass --org?")}
				}

				if opts.Visibility != shared.All && opts.Visibility != shared.Private && opts.Visibility != shared.Selected {
					return &cmdutil.FlagError{Err: errors.New(
						"--visibility must be one of `all`, `private`, or `selected`")}
				}

				if opts.Visibility != shared.Selected && cmd.Flags().Changed("repos") {
					return &cmdutil.FlagError{Err: errors.New(
						"--repos only supported when --visibility='selected'")}
				}

				if opts.Visibility == shared.Selected && !cmd.Flags().Changed("repos") {
					return &cmdutil.FlagError{Err: errors.New(
						"--repos flag required when --visibility='selected'")}
				}
			} else {
				if cmd.Flags().Changed("repos") {
					opts.Visibility = shared.Selected
				}
			}

			if runF != nil {
				return runF(opts)
			}

			return setRun(opts)
		},
	}
	cmd.Flags().StringVarP(&opts.OrgName, "org", "o", "", "Set a secret for an organization")
	cmd.Flags().StringVarP(&opts.EnvName, "env", "e", "", "Set a secret for an organization")
	cmd.Flags().StringVarP(&opts.Visibility, "visibility", "v", "private", "Set visibility for an organization secret: `all`, `private`, or `selected`")
	cmd.Flags().StringSliceVarP(&opts.RepositoryNames, "repos", "r", []string{}, "List of repository names for `selected` visibility")
	cmd.Flags().StringVarP(&opts.Body, "body", "b", "", "A value for the secret. Reads from STDIN if not specified.")

	return cmd
}

func setRun(opts *SetOptions) error {
	body, err := getBody(opts)
	if err != nil {
		return fmt.Errorf("did not understand secret body: %w", err)
	}

	c, err := opts.HttpClient()
	if err != nil {
		return fmt.Errorf("could not create http client: %w", err)
	}
	client := api.NewClientFromHTTP(c)

	orgName := opts.OrgName
	envName := opts.EnvName

	var baseRepo ghrepo.Interface
	if orgName == "" {
		baseRepo, err = opts.BaseRepo()
		if err != nil {
			return fmt.Errorf("could not determine base repo: %w", err)
		}
	}

	cfg, err := opts.Config()
	if err != nil {
		return err
	}

	host, err := cfg.DefaultHost()
	if err != nil {
		return err
	}

	var pk *PubKey
	if orgName != "" {
		pk, err = getOrgPublicKey(client, host, orgName)
	} else {
		pk, err = getRepoPubKey(client, baseRepo)
	}
	if err != nil {
		return fmt.Errorf("failed to fetch public key: %w", err)
	}

	eBody, err := box.SealAnonymous(nil, body, &pk.Raw, opts.RandomOverride)
	if err != nil {
		return fmt.Errorf("failed to encrypt body: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(eBody)

	if orgName != "" {
		err = putOrgSecret(client, host, pk, *opts, encoded)
	} else if envName != "" {
		err = putEnvSecret(client, pk, baseRepo, envName, opts.SecretName, encoded)
	} else {
		err = putRepoSecret(client, pk, baseRepo, opts.SecretName, encoded)
	}
	if err != nil {
		return fmt.Errorf("failed to set secret: %w", err)
	}

	if opts.IO.IsStdoutTTY() {
		target := orgName
		if orgName == "" {
			target = ghrepo.FullName(baseRepo)
		}
		cs := opts.IO.ColorScheme()
		fmt.Fprintf(opts.IO.Out, "%s Set secret %s for %s\n", cs.SuccessIconWithColor(cs.Green), opts.SecretName, target)
	}

	return nil
}

func validSecretName(name string) error {
	if name == "" {
		return errors.New("secret name cannot be blank")
	}

	if strings.HasPrefix(name, "GITHUB_") {
		return errors.New("secret name cannot begin with GITHUB_")
	}

	leadingNumber := regexp.MustCompile(`^[0-9]`)
	if leadingNumber.MatchString(name) {
		return errors.New("secret name cannot start with a number")
	}

	validChars := regexp.MustCompile(`^([0-9]|[a-z]|[A-Z]|_)+$`)
	if !validChars.MatchString(name) {
		return errors.New("secret name can only contain letters, numbers, and _")
	}

	return nil
}

func getBody(opts *SetOptions) ([]byte, error) {
	if opts.Body == "" {
		if opts.IO.CanPrompt() {
			err := prompt.SurveyAskOne(&survey.Password{
				Message: "Paste your secret",
			}, &opts.Body)
			if err != nil {
				return nil, err
			}
			fmt.Fprintln(opts.IO.Out)
		} else {
			body, err := ioutil.ReadAll(opts.IO.In)
			if err != nil {
				return nil, fmt.Errorf("failed to read from STDIN: %w", err)
			}

			return body, nil
		}
	}

	return []byte(opts.Body), nil
}
