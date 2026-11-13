//go:build nobot

package main

import "context"

func startBot(context.Context, *Config, *Reminders, func(context.Context) string, func(string) (string, bool)) error {
	return errBotDisabled
}
