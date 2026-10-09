//go:build !nobot

package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

type discordSender struct{ s *discordgo.Session }

func (d discordSender) Send(_ context.Context, channelID, text string, mentionUserIDs []string) (string, error) {
	msg := &discordgo.MessageSend{Content: text, AllowedMentions: &discordgo.MessageAllowedMentions{Users: mentionUserIDs}}
	sent, err := d.s.ChannelMessageSendComplex(channelID, msg)
	if err != nil {
		return "", err
	}
	return sent.ID, nil
}

// startBot は Discord Bot に接続し、指定チャンネルのメッセージに応答する。ctx が終わるまで戻らない。
// assignments は「課題」と送られたときの返事を作る関数（nil なら使わない）。
func startBot(ctx context.Context, cfg *Config, rem *Reminders, assignments func(context.Context) string) error {
	s, err := discordgo.New("Bot " + cfg.BotToken)
	if err != nil {
		return err
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("Discord Bot に接続しました: %s", r.User.Username)
		if len(cfg.RemindMentions) > 0 {
			rem.SetMentions(resolveMentions(s, cfg.RemindChannelID, cfg.RemindMentions))
		}
	})
	s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author == nil || m.Author.Bot || m.ChannelID != cfg.RemindChannelID {
			return
		}
		var reply string
		if assignments != nil && isAssignmentsCommand(m.Content) {
			_ = s.ChannelTyping(m.ChannelID)
			c, cancel := context.WithTimeout(ctx, time.Minute)
			reply = assignments(c)
			cancel()
		} else {
			replyTo := ""
			if m.MessageReference != nil {
				replyTo = m.MessageReference.MessageID
			}
			reply = rem.Handle(m.Author.ID, m.ChannelID, m.Content, replyTo)
		}
		if reply == "" {
			return
		}
		_, err := s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content:         reply,
			Reference:       m.Reference(),
			AllowedMentions: &discordgo.MessageAllowedMentions{RepliedUser: false},
		})
		if err != nil {
			log.Printf("返信に失敗: %v", err)
		}
	})

	if err := s.Open(); err != nil {
		return fmt.Errorf("Discord Bot に接続できません（トークンと Message Content Intent を確認してください）: %w", err)
	}
	defer s.Close()

	rem.Run(ctx, discordSender{s})
	return nil
}

// resolveMentions はユーザー名（または ID）を、チャンネルがあるサーバーのメンバーから探して ID にする。
func resolveMentions(s *discordgo.Session, channelID string, names []string) []string {
	ch, err := s.Channel(channelID)
	if err != nil {
		log.Printf("チャンネル情報を取得できません（メンションは依頼した本人だけにします）: %v", err)
		return nil
	}
	var ids []string
	for _, name := range names {
		if isAllDigits(name) {
			ids = append(ids, name)
			continue
		}
		members, err := s.GuildMembersSearch(ch.GuildID, name, 10)
		if err != nil {
			log.Printf("メンバー %q を検索できません: %v", name, err)
			continue
		}
		found := ""
		for _, m := range members {
			if m.User != nil && strings.EqualFold(m.User.Username, name) {
				found = m.User.ID
				break
			}
		}
		if found == "" {
			log.Printf("メンバー %q が見つかりません（ユーザー名を確認してください）", name)
			continue
		}
		log.Printf("メンション先: %s (%s)", name, found)
		ids = append(ids, found)
	}
	return ids
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
