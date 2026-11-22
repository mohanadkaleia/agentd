package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"agentd/internal/app"
	"agentd/internal/integrations/telegram"
	"agentd/internal/tools"
)

const telegramTokenAccount = "telegram-bot-token"

const channelUsage = `usage:
  agentd channel pair telegram       print a one-time code to send to your bot (valid 10 minutes)
  agentd channel list                paired channels and their settings
  agentd channel set telegram [-level minimal|summary|full] [-widen] [-no-decide]
  agentd channel revoke telegram     unpair at once (a lost phone)

Store the bot token first: security add-generic-password -s agentd -a telegram-bot-token -w`

func channelCommand(ctx context.Context, db *sql.DB, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", channelUsage)
	}
	a := app.New(db, nil, "", tools.Registry{}, time.Now)
	switch args[0] {
	case "pair":
		if len(args) != 2 || args[1] != telegram.Name {
			return fmt.Errorf("%s", channelUsage)
		}
		code, err := a.Chat.Pair(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Send this code to your bot from your phone within 10 minutes: %s\n", code)
		if keychainSecret("agentd", telegramTokenAccount) == "" {
			fmt.Println("No bot token is stored yet: security add-generic-password -s agentd -a telegram-bot-token -w")
		}
		fmt.Println("agentd serve must be running to receive it.")
		return nil
	case "list":
		channels, err := a.Chat.Channels(ctx)
		if err != nil {
			return err
		}
		if len(channels) == 0 {
			fmt.Println("No channels. Start with: agentd channel pair telegram")
		}
		for _, c := range channels {
			state := "not paired"
			if c.Paired {
				state = "paired " + c.PairedAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Printf("%-10s %-24s level %-8s decide %-5v widen %v\n", c.Name, state, c.Level, c.Decide, c.Widen)
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return fmt.Errorf("%s", channelUsage)
		}
		if err := a.Chat.Revoke(ctx, args[1]); err != nil {
			return err
		}
		fmt.Println("Unpaired. Messages from that phone are ignored from now on.")
		return nil
	case "set":
		if len(args) < 2 {
			return fmt.Errorf("%s", channelUsage)
		}
		c, err := a.Chat.Channel(ctx, args[1])
		if err != nil {
			return err
		}
		f := flag.NewFlagSet("channel set", flag.ContinueOnError)
		level := f.String("level", c.Level, "minimal, summary or full")
		widen := f.Bool("widen", false, "allow approving proposals, /resume and /unhalt from the phone")
		noDecide := f.Bool("no-decide", false, "turn off approving, rejecting and answering from the phone")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		return a.Chat.Configure(ctx, args[1], *level, *widen, !*noDecide)
	}
	return fmt.Errorf("%s", channelUsage)
}

// startTelegram runs the Telegram channel when a bot token is stored.
func startTelegram(ctx context.Context, a *app.App, log *slog.Logger) {
	token := keychainSecret("agentd", telegramTokenAccount)
	if token == "" {
		return
	}
	base := os.Getenv("AGENTD_TELEGRAM_API")
	log.Info("telegram channel running")
	go (&telegram.Bot{Token: token, BaseURL: base, Chat: a.Chat, Log: log}).Run(ctx)
}
