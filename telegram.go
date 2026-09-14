package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// TelegramBot menyimpan token bot, HTTP client, dan offset untuk long polling
type TelegramBot struct {
	Token  string
	client *http.Client
	offset int64
}

// NewTelegramBot membuat instance baru dari TelegramBot dengan timeout HTTP client
func NewTelegramBot(token string) *TelegramBot {
	return &TelegramBot{
		Token: token,
		client: &http.Client{
			Timeout: 60 * time.Second, // Timeout untuk long polling
		},
	}
}

// Struct-struct untuk memetakan respons JSON dari API Telegram
type UpdateResponse struct {
	Ok     bool     `json:"ok"`
	Result []Update `json:"result"`
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      *Chat  `json:"chat"`
	Text      string `json:"text"`
}

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// SetMyCommands mengatur daftar perintah (commands) pada menu bot Telegram
func (b *TelegramBot) SetMyCommands(commands []map[string]string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/setMyCommands", b.Token)
	
	payload := map[string]interface{}{
		"commands": commands,
	}
	body, _ := json.Marshal(payload)
	
	resp, err := b.client.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// GetMe mengambil informasi tentang akun bot itu sendiri
func (b *TelegramBot) GetMe() (*User, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", b.Token)
	
	resp, err := b.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var apiResp struct {
		Ok     bool  `json:"ok"`
		Result *User `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}
	if !apiResp.Ok {
		return nil, fmt.Errorf("API Telegram mengembalikan ok=false pada getMe")
	}
	return apiResp.Result, nil
}

// GetUpdates mengambil pesan/update baru melalui mekanisme long polling
func (b *TelegramBot) GetUpdates() ([]Update, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates", b.Token)
	
	payload := map[string]interface{}{
		"offset":  b.offset,
		"timeout": 30, // Long polling selama 30 detik
	}
	body, _ := json.Marshal(payload)
	
	resp, err := b.client.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var apiResp UpdateResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}

	if !apiResp.Ok {
		return nil, fmt.Errorf("API Telegram mengembalikan ok=false")
	}

	// Perbarui offset ke update_id tertinggi + 1 agar pesan tidak berulang
	for _, u := range apiResp.Result {
		if u.UpdateID >= b.offset {
			b.offset = u.UpdateID + 1
		}
	}

	return apiResp.Result, nil
}

// SendMessage mengirim pesan teks ke ID chat tertentu
func (b *TelegramBot) SendMessage(chatID int64, text string, parseMode ...string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.Token)
	
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	if len(parseMode) > 0 && parseMode[0] != "" {
		payload["parse_mode"] = parseMode[0]
	}
	body, _ := json.Marshal(payload)
	
	resp, err := b.client.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API Telegram mengembalikan status %d: %s", resp.StatusCode, string(respBody))
	}
	
	return nil
}

// SendChatAction mengirim indikator status ke pengguna (misalnya indikator "typing")
func (b *TelegramBot) SendChatAction(chatID int64, action string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", b.Token)
	
	payload := map[string]interface{}{
		"chat_id": chatID,
		"action":  action,
	}
	body, _ := json.Marshal(payload)
	
	resp, err := b.client.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API Telegram SendChatAction mengembalikan status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
