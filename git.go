package envbuilder

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/skeema/knownhosts"
	gossh "golang.org/x/crypto/ssh"
)

type CloneRepoOptions struct {
	Path    string
	Storage billy.Filesystem

	RepoURL      string
	RepoAuth     transport.AuthMethod
	Progress     sideband.Progress
	Insecure     bool
	SingleBranch bool
	Depth        int
	CABundle     []byte
	ProxyOptions transport.ProxyOptions
}

// CloneRepo will clone the repository at the given URL into the given path.
// If a repository is already initialized at the given path, it will not
// be cloned again.
//
// The bool returned states whether the repository was cloned or not.
func CloneRepo(ctx context.Context, opts CloneRepoOptions) (bool, error) {
	parsed, err := url.Parse(opts.RepoURL)
	if err != nil {
		return false, fmt.Errorf("parse url %q: %w", opts.RepoURL, err)
	}
	if parsed.Hostname() == "dev.azure.com" {
		// Azure DevOps requires capabilities multi_ack / multi_ack_detailed,
		// which are not fully implemented and by default are included in
		// transport.UnsupportedCapabilities.
		//
		// The initial clone operations require a full download of the repository,
		// and therefore those unsupported capabilities are not as crucial, so
		// by removing them from that list allows for the first clone to work
		// successfully.
		//
		// Additional fetches will yield issues, therefore work always from a clean
		// clone until those capabilities are fully supported.
		//
		// New commits and pushes against a remote worked without any issues.
		// See: https://github.com/go-git/go-git/issues/64
		//
		// This is knowingly not safe to call in parallel, but it seemed
		// like the least-janky place to add a super janky hack.
		transport.UnsupportedCapabilities = []capability.Capability{
			capability.ThinPack,
		}
	}

	err = opts.Storage.MkdirAll(opts.Path, 0755)
	if err != nil {
		return false, fmt.Errorf("mkdir %q: %w", opts.Path, err)
	}
	reference := parsed.Fragment
	if reference == "" && opts.SingleBranch {
		reference = "refs/heads/main"
	}
	parsed.RawFragment = ""
	parsed.Fragment = ""
	fs, err := opts.Storage.Chroot(opts.Path)
	if err != nil {
		return false, fmt.Errorf("chroot %q: %w", opts.Path, err)
	}
	gitDir, err := fs.Chroot(".git")
	if err != nil {
		return false, fmt.Errorf("chroot .git: %w", err)
	}
	gitStorage := filesystem.NewStorage(gitDir, cache.NewObjectLRU(cache.DefaultMaxSize*10))
	fsStorage := filesystem.NewStorage(fs, cache.NewObjectLRU(cache.DefaultMaxSize*10))
	repo, err := git.Open(fsStorage, gitDir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		err = nil
	}
	if err != nil {
		return false, fmt.Errorf("open %q: %w", opts.RepoURL, err)
	}
	if repo != nil {
		return false, nil
	}

	_, err = git.CloneContext(ctx, gitStorage, fs, &git.CloneOptions{
		URL:             parsed.String(),
		Auth:            opts.RepoAuth,
		Progress:        opts.Progress,
		ReferenceName:   plumbing.ReferenceName(reference),
		InsecureSkipTLS: opts.Insecure,
		Depth:           opts.Depth,
		SingleBranch:    opts.SingleBranch,
		CABundle:        opts.CABundle,
		ProxyOptions:    opts.ProxyOptions,
	})
	if errors.Is(err, git.ErrRepositoryAlreadyExists) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("clone %q: %w", opts.RepoURL, err)
	}
	return true, nil
}

// ReadPrivateKey attempts to read an SSH private key from path
// and returns an ssh.Signer.
func ReadPrivateKey(path string) (gossh.Signer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open private key file: %w", err)
	}
	defer f.Close()
	bs, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read private key file: %w", err)
	}
	k, err := gossh.ParsePrivateKey(bs)
	if err != nil {
		return nil, fmt.Errorf("parse private key file: %w", err)
	}
	return k, nil
}

// LogHostKeyCallback is a HostKeyCallback that just logs host keys
// and does nothing else.
func LogHostKeyCallback(log LoggerFunc) gossh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		var sb strings.Builder
		_ = knownhosts.WriteKnownHost(&sb, hostname, remote, key)
		// skeema/knownhosts uses a fake public key to determine the host key
		// algorithms. Ignore this one.
		if s := sb.String(); !strings.Contains(s, "fake-public-key ZmFrZSBwdWJsaWMga2V5") {
			log(codersdk.LogLevelInfo, "#1: 🔑 Got host key: %s", strings.TrimSpace(s))
		}
		return nil
	}
}

// SetupRepoAuth determines the desired AuthMethod based on options.GitURL:
//
// | Git URL format          | GIT_USERNAME | GIT_PASSWORD | Auth Method |
// | ------------------------|--------------|--------------|-------------|
// | https?://host.tld/repo  | Not Set      | Not Set      | None        |
// | https?://host.tld/repo  | Not Set      | Set          | HTTP Basic  |
// | https?://host.tld/repo  | Set          | Not Set      | HTTP Basic  |
// | https?://host.tld/repo  | Set          | Set          | HTTP Basic  |
// | All other formats       | -            | -            | SSH         |
//
// For SSH authentication, the default username is "git" but will honour
// GIT_USERNAME if set.
//
// If SSH_PRIVATE_KEY_PATH is set, an SSH private key will be read from
// that path and the SSH auth method will be configured with that key.
//
// If no SSH_PRIVATE_KEY_PATH is set, but CODER_AGENT_URL and CODER_AGENT_TOKEN
// are both specified, envbuilder will attempt to fetch the corresponding
// Git SSH key for the user.
//
// Otherwise, SSH authentication will fall back to SSH_AUTH_SOCK, in which
// case SSH_AUTH_SOCK must be set to the path of a listening SSH agent socket.
//
// If SSH_KNOWN_HOSTS is not set, the SSH auth method will be configured
// to accept and log all host keys. Otherwise, host key checking will be
// performed as usual.
func SetupRepoAuth(ctx context.Context, options *Options) transport.AuthMethod {
	if options.GitURL == "" {
		options.Logger(codersdk.LogLevelInfo, "#1: ❔ No Git URL supplied!")
		return nil
	}
	if strings.HasPrefix(options.GitURL, "http://") || strings.HasPrefix(options.GitURL, "https://") {
		// Special case: no auth
		if options.GitUsername == "" && options.GitPassword == "" {
			options.Logger(codersdk.LogLevelInfo, "#1: 👤 Using no authentication!")
			return nil
		}
		// Basic Auth
		// NOTE: we previously inserted the credentials into the repo URL.
		// This was removed in https://github.com/coder/envbuilder/pull/141
		options.Logger(codersdk.LogLevelInfo, "#1: 🔒 Using HTTP basic authentication!")
		return &githttp.BasicAuth{
			Username: options.GitUsername,
			Password: options.GitPassword,
		}
	}

	// Generally git clones over SSH use the 'git' user, but respect
	// GIT_USERNAME if set.
	if options.GitUsername == "" {
		options.GitUsername = "git"
	}

	// Assume SSH auth for all other formats.
	options.Logger(codersdk.LogLevelInfo, "#1: 🔑 Using SSH authentication!")

	var signer gossh.Signer
	if options.GitSSHPrivateKeyPath != "" {
		s, err := ReadPrivateKey(options.GitSSHPrivateKeyPath)
		if err != nil {
			options.Logger(codersdk.LogLevelError, "#1: ❌ Failed to read private key from %s: %s", options.GitSSHPrivateKeyPath, err.Error())
		} else {
			signer = s
			options.Logger(codersdk.LogLevelInfo, "#1: 🔑 Using %s key %s!", s.PublicKey().Type(), keyFingerprint(signer)[:8])
		}
	}

	// If we have no signer but we have a Coder URL and agent token, try to fetch
	// an SSH key from Coder!
	if signer == nil && options.CoderAgentURL != "" && options.CoderAgentToken != "" {
		options.Logger(codersdk.LogLevelInfo, "#1: 🔑 Fetching key from %s!", options.CoderAgentURL)
		fetchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		s, err := FetchCoderSSHKeyRetry(fetchCtx, options.Logger, options.CoderAgentURL, options.CoderAgentToken)
		if err == nil {
			signer = s
			options.Logger(codersdk.LogLevelInfo, "#1: 🔑 Fetched %s key %s !", signer.PublicKey().Type(), keyFingerprint(signer)[:8])
		} else {
			options.Logger(codersdk.LogLevelInfo, "#1: ❌ Failed to fetch SSH key from %s: %w", options.CoderAgentURL, err)
		}
	}

	// If no SSH key set, fall back to agent auth.
	if signer == nil {
		options.Logger(codersdk.LogLevelError, "#1: 🔑 No SSH key found, falling back to agent!")
		auth, err := gitssh.NewSSHAgentAuth(options.GitUsername)
		if err != nil {
			options.Logger(codersdk.LogLevelError, "#1: ❌ Failed to connect to SSH agent: %s", err.Error())
			return nil // nothing else we can do
		}
		if os.Getenv("SSH_KNOWN_HOSTS") == "" {
			options.Logger(codersdk.LogLevelWarn, "#1: 🔓 SSH_KNOWN_HOSTS not set, accepting all host keys!")
			auth.HostKeyCallback = LogHostKeyCallback(options.Logger)
		}
		return auth
	}

	auth := &gitssh.PublicKeys{
		User:   options.GitUsername,
		Signer: signer,
	}

	// Generally git clones over SSH use the 'git' user, but respect
	// GIT_USERNAME if set.
	if auth.User == "" {
		auth.User = "git"
	}

	// Duplicated code due to Go's type system.
	if os.Getenv("SSH_KNOWN_HOSTS") == "" {
		options.Logger(codersdk.LogLevelWarn, "#1: 🔓 SSH_KNOWN_HOSTS not set, accepting all host keys!")
		auth.HostKeyCallback = LogHostKeyCallback(options.Logger)
	}
	return auth
}

// FetchCoderSSHKeyRetry wraps FetchCoderSSHKey in backoff.Retry.
// Retries are attempted if Coder responds with a 401 Unauthorized.
// This indicates that the workspace build has not yet completed.
// It will retry for up to 1 minute with exponential backoff.
// Any other error is considered a permanent failure.
func FetchCoderSSHKeyRetry(ctx context.Context, log LoggerFunc, coderURL, agentToken string) (gossh.Signer, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	signerChan := make(chan gossh.Signer, 1)
	eb := backoff.NewExponentialBackOff()
	eb.MaxElapsedTime = 0
	eb.MaxInterval = time.Minute
	bkoff := backoff.WithContext(eb, ctx)
	err := backoff.Retry(func() error {
		s, err := FetchCoderSSHKey(ctx, coderURL, agentToken)
		if err != nil {
			var sdkErr *codersdk.Error
			if errors.As(err, &sdkErr) && sdkErr.StatusCode() == http.StatusUnauthorized {
				// Retry, as this may just mean that the workspace build has not yet
				// completed.
				log(codersdk.LogLevelInfo, "#1: 🕐 Backing off as the workspace build has not yet completed...")
				return err
			}
			close(signerChan)
			return backoff.Permanent(err)
		}
		signerChan <- s
		return nil
	}, bkoff)
	return <-signerChan, err
}

// FetchCoderSSHKey fetches the user's Git SSH key from Coder using the supplied
// Coder URL and agent token.
func FetchCoderSSHKey(ctx context.Context, coderURL string, agentToken string) (gossh.Signer, error) {
	u, err := url.Parse(coderURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Coder URL: %w", err)
	}
	client := agentsdk.New(u)
	client.SetSessionToken(agentToken)
	key, err := client.GitSSHKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("get coder ssh key: %w", err)
	}
	signer, err := gossh.ParsePrivateKey([]byte(key.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("parse coder ssh key: %w", err)
	}
	return signer, nil
}

// keyFingerprint returns the md5 checksum of the public key of signer.
func keyFingerprint(s gossh.Signer) string {
	h := md5.New()
	h.Write(s.PublicKey().Marshal())
	return fmt.Sprintf("%x", h.Sum(nil))
}
