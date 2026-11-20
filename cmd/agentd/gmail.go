package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"agentd/internal/app"
	"agentd/internal/integrations/gmail"
	"agentd/internal/tools"
)

const gmailKeychainAccount = "gmail-oauth"

func loadGmailCredentials() (gmail.Credentials, bool) {
	raw, ok := keychainLoadHex("agentd", gmailKeychainAccount)
	if !ok {
		return gmail.Credentials{}, false
	}
	var c gmail.Credentials
	if err := json.Unmarshal(raw, &c); err != nil || c.RefreshToken == "" {
		return gmail.Credentials{}, false
	}
	return c, true
}

func gmailAccount(ctx context.Context) (*gmail.Account, error) {
	creds, ok := loadGmailCredentials()
	if !ok {
		return nil, nil
	}
	client := &gmail.Client{HTTP: creds.HTTPClient(ctx)}
	profile, err := client.Profile(ctx)
	if err != nil {
		return nil, err
	}
	return &gmail.Account{Client: client, Email: profile.EmailAddress, Now: time.Now, ReconcileTries: 3, ReconcileWait: 2 * time.Second}, nil
}

func gmailTools(ctx context.Context, log *slog.Logger) (*gmail.Account, []tools.Tool) {
	account, err := gmailAccount(ctx)
	if err != nil {
		log.Error("gmail unavailable; continuing without it", "err", err)
		return nil, nil
	}
	if account == nil {
		return nil, nil
	}
	log.Info("gmail connected", "account", account.Email)
	return account, account.Tools()
}

func watchGmail(ctx context.Context, a *app.App, account *gmail.Account, every time.Duration, log *slog.Logger) {
	if account == nil || every <= 0 {
		return
	}
	w := &gmail.Watcher{Account: account, DB: a.DB, Scheduler: a.Scheduler}
	go func() {
		for ctx.Err() == nil {
			if n, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
				log.Error("gmail watch", "err", err)
			} else if n > 0 {
				log.Info("gmail events", "new", n)
			}
			select {
			case <-ctx.Done():
			case <-time.After(every):
			}
		}
	}()
}

func gmailCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agentd gmail auth -client-secret FILE | agentd gmail status")
	}
	fs := flag.NewFlagSet("gmail "+args[0], flag.ContinueOnError)
	secretFile := fs.String("client-secret", "", "OAuth client JSON downloaded from Google Cloud (Desktop app type)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "auth":
		if *secretFile == "" {
			return errors.New("-client-secret is required")
		}
		creds, err := gmail.ReadClientSecret(*secretFile)
		if err != nil {
			return err
		}
		authCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		creds, err = gmail.Authorize(authCtx, creds, func(url string) error {
			fmt.Println("Opening your browser to connect Gmail. If it does not open, visit:\n" + url)
			return exec.Command("/usr/bin/open", url).Run()
		})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(creds)
		if err := keychainStoreHex("agentd", gmailKeychainAccount, raw); err != nil {
			return err
		}
		fmt.Println("Gmail connected; credentials stored in the login keychain. Restart agentd to use it.")
		return nil
	case "status":
		account, err := gmailAccount(ctx)
		if err != nil {
			return err
		}
		if account == nil {
			fmt.Println("gmail: not connected (run: agentd gmail auth -client-secret FILE)")
			return nil
		}
		fmt.Println("gmail: connected as", account.Email)
		return nil
	}
	return fmt.Errorf("unknown gmail command %q", args[0])
}
