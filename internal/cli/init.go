package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/s4chdev/voxen/internal/config"
	"github.com/s4chdev/voxen/internal/identity"
	"github.com/s4chdev/voxen/internal/ipc"
)

func (a *App) newInitCommand() *cobra.Command {
	var (
		displayName string
		signaling   string
		force       bool
		noConfig    bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the local identity and configuration",
		Long: `Create an Ed25519 identity, an explicit trust store and a configuration file.

The identity fingerprint is printed once creation succeeds. Share it with peers
over a channel you already trust; they will verify it with "voxen verify" before
any audio is sent between you.

If VOXEN_KEY_PASSPHRASE is set, the private key is sealed with XChaCha20-Poly1305
under a scrypt-derived key. Without it, the key file is protected by 0600
permissions only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.paths.Ensure(); err != nil {
				return err
			}
			if strings.TrimSpace(displayName) == "" {
				displayName = defaultDisplayName()
			}

			// Refuse to clobber existing state unless the operator asked for it.
			keyPath := a.paths.IdentityFile
			cfgPath := a.paths.ConfigFile
			if !noConfig {
				if _, err := os.Stat(cfgPath); err == nil && !force {
					return exitWith(ExitUsage,
						fmt.Errorf("configuration already exists at %s; use --force to overwrite", cfgPath))
				}
			}

			cfg := config.Default()
			cfg.Identity.DisplayName = strings.TrimSpace(displayName)
			if signaling != "" {
				cfg.Signaling.URL = signaling
			}
			if err := cfg.Validate(); err != nil {
				return exitWith(ExitUsage, err)
			}

			store := identity.StoreFromEnv(keyPath)
			exists, err := store.Exists()
			if err != nil {
				return err
			}
			var id *identity.Identity
			var rotatedFrom string
			if exists {
				if !force {
					return exitWith(ExitUsage, fmt.Errorf(
						"an identity already exists at %s; rotating it changes your fingerprint for every peer.\n"+
							"Re-run with --force only if that is intended", keyPath))
				}
				// Record the old fingerprint before deleting: a rotation must be
				// visible in logs and in the output.
				old, loadErr := identity.StoreFromEnv(keyPath).Load()
				if loadErr == nil {
					rotatedFrom = old.Fingerprint().String()
				}
				if err := store.Remove(); err != nil {
					return err
				}
			}

			id, err = store.Create(cfg.Identity.DisplayName)
			if err != nil {
				return err
			}
			// Create the trust store eagerly so a later unwritable directory is
			// caught now, not during the first call.
			if _, err := identity.OpenTrust(filepath.Dir(keyPath)); err != nil {
				return err
			}

			if !noConfig {
				if err := config.Save(cfgPath, cfg); err != nil {
					return err
				}
			}

			if a.jsonOut {
				return a.printJSON(map[string]any{
					"fingerprint":   id.Fingerprint().String(),
					"public_key":    id.PublicKeyEncoded(),
					"config_path":   cfgPath,
					"identity_path": keyPath,
					"encrypted":     os.Getenv(identity.PassphraseEnv) != "",
					"rotated_from":  rotatedFrom,
				})
			}

			fmt.Fprintln(a.stdout, "voxen is initialised.")
			fmt.Fprintln(a.stdout)
			fmt.Fprintf(a.stdout, "  identity:     %s\n", keyPath)
			fmt.Fprintf(a.stdout, "  configuration:%s\n", cfgPath)
			if os.Getenv(identity.PassphraseEnv) != "" {
				fmt.Fprintln(a.stdout, "  key storage:  encrypted (XChaCha20-Poly1305, scrypt)")
			} else {
				fmt.Fprintln(a.stdout, "  key storage:  file permissions only (0600)")
				fmt.Fprintln(a.stdout, "                set VOXEN_KEY_PASSPHRASE and re-run with --force to encrypt")
			}
			fmt.Fprintln(a.stdout)
			fmt.Fprintf(a.stdout, "  your fingerprint:\n\n    %s\n\n", id.Fingerprint().String())
			if rotatedFrom != "" {
				fmt.Fprintf(a.stdout, "  previous fingerprint (now retired):\n    %s\n\n", rotatedFrom)
			}
			fmt.Fprintln(a.stdout, "Read this fingerprint to each peer over a channel you already trust.")
			fmt.Fprintln(a.stdout, "They must run:  voxen verify "+id.Fingerprint().Short())
			fmt.Fprintln(a.stdout)
			fmt.Fprintln(a.stdout, "Next: voxen start")
			return nil
		},
	}
	cmd.Flags().StringVar(&displayName, "name", "", "display name shown to peers (default: your username)")
	cmd.Flags().StringVar(&signaling, "signaling", "", "signaling server URL (default: ws://127.0.0.1:8080/ws)")
	cmd.Flags().BoolVar(&force, "force", false, "rotate an existing identity and overwrite the configuration")
	cmd.Flags().BoolVar(&noConfig, "no-config", false, "create the identity without writing a configuration file")
	return cmd
}

func (a *App) newIdentityCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "identity",
		Short: "Show the local identity fingerprint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Prefer the daemon: it holds the loaded identity and can report
			// whether a passphrase was supplied to unlock it.
			var res ipc.IdentityResult
			if err := a.call(ipc.MethodIdentityShow, nil, &res); err == nil {
				if a.jsonOut {
					return a.printJSON(res)
				}
				fmt.Fprintf(a.stdout, "fingerprint:  %s\n", res.Fingerprint)
				if res.DisplayName != "" {
					fmt.Fprintf(a.stdout, "display name: %s\n", res.DisplayName)
				}
				if res.CreatedAt != "" {
					fmt.Fprintf(a.stdout, "created:      %s\n", res.CreatedAt)
				}
				fmt.Fprintf(a.stdout, "key storage:  %s\n", encryptionLabel(res.Encrypted))
				return nil
			}

			// Fall back to reading the key file locally so "voxen identity" works
			// before the daemon is running.
			if err := a.loadLocalIdentity(); err != nil {
				return err
			}
			id := a.localIdentity
			if a.jsonOut {
				return a.printJSON(map[string]string{
					"fingerprint": id.Fingerprint().String(),
					"public_key":  id.PublicKeyEncoded(),
					"created_at":  id.CreatedAt(),
				})
			}
			fmt.Fprintf(a.stdout, "fingerprint:  %s\n", id.Fingerprint().String())
			if id.DisplayName != "" {
				fmt.Fprintf(a.stdout, "display name: %s\n", id.DisplayName)
			}
			if id.CreatedAt() != "" {
				fmt.Fprintf(a.stdout, "created:      %s\n", id.CreatedAt())
			}
			return nil
		},
	}
	return cmd
}

func encryptionLabel(encrypted bool) string {
	if encrypted {
		return "encrypted (XChaCha20-Poly1305)"
	}
	return "file permissions only (0600)"
}

// defaultDisplayName derives a friendly name from the account.
func defaultDisplayName() string {
	for _, env := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	return "voxen-user"
}

// loadLocalIdentity loads the identity file directly, for commands used before
// the daemon starts.
func (a *App) loadLocalIdentity() error {
	if a.localIdentity != nil {
		return nil
	}
	keyPath := a.paths.IdentityFile
	if a.cfg.Identity.File != "" {
		keyPath = a.cfg.Identity.File
	} else if err := a.loadLocalConfig(); err == nil && a.cfg.Identity.File != "" {
		keyPath = a.cfg.Identity.File
	}
	store := identity.StoreFromEnv(keyPath)
	id, err := store.Load()
	if err != nil {
		if errors.Is(err, identity.ErrNoIdentity) {
			return exitWith(ExitUsage, errors.New("no identity found; run 'voxen init' first"))
		}
		return err
	}
	a.localIdentity = id
	return nil
}
