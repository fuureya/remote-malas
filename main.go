package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/joho/godotenv"
)

// SessionState menyimpan konfigurasi dan riwayat sesi pengguna
type SessionState struct {
	WorkingDir string
	Continue   bool
	IsBusy     bool
	LastActive time.Time
}

var (
	sessions = make(map[int64]*SessionState)
	mu       sync.RWMutex

	allowedDirs []string
	defaultDir  string

	allowedUserIDs = make(map[int64]bool)
	allowedChatIDs = make(map[int64]bool)

	botUsername string
)

func main() {
	// Memuat variabel lingkungan dari file .env
	err := godotenv.Load()
	if err != nil {
		log.Println("File .env tidak ditemukan, membaca dari variabel lingkungan sistem")
	}

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("Variabel lingkungan TELEGRAM_BOT_TOKEN belum diatur")
	}

	// Membaca dan memparsing ALLOWED_USER_IDS (Whitelist Pengguna)
	users := os.Getenv("ALLOWED_USER_IDS")
	if users != "" {
		for _, u := range strings.Split(users, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				id, err := strconv.ParseInt(u, 10, 64)
				if err == nil {
					allowedUserIDs[id] = true
				}
			}
		}
	}

	// Membaca dan memparsing ALLOWED_CHAT_IDS (Whitelist Chat/Grup)
	chats := os.Getenv("ALLOWED_CHAT_IDS")
	if chats != "" {
		for _, c := range strings.Split(chats, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				id, err := strconv.ParseInt(c, 10, 64)
				if err == nil {
					allowedChatIDs[id] = true
				}
			}
		}
	}

	// Membaca dan memparsing ALLOWED_DIRS untuk Sandbox Direktori
	dirs := os.Getenv("ALLOWED_DIRS")
	if dirs != "" {
		for _, d := range strings.Split(dirs, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				absD, err := filepath.Abs(d)
				if err == nil {
					allowedDirs = append(allowedDirs, absD)
				}
			}
		}
	}

	if len(allowedDirs) > 0 {
		defaultDir = allowedDirs[0]
	} else {
		// Jika tidak diatur, gunakan direktori kerja saat ini sebagai fallback
		defaultDir, _ = os.Getwd()
		allowedDirs = append(allowedDirs, defaultDir)
	}

	log.Printf("Akar Sandbox: %v", allowedDirs)

	bot := NewTelegramBot(token)

	// Mengatur menu perintah (commands) pada bot Telegram
	commands := []map[string]string{
		{"command": "info", "description": "Lihat panduan lengkap dan daftar perintah bot"},
		{"command": "new", "description": "Memulai percakapan baru dengan Antigravity (membersihkan sesi saat ini)"},
		{"command": "dir", "description": "Mengubah direktori kerja (Sandbox) - Contoh: /dir /path/to/dir"},
		{"command": "goal", "description": "Menjalankan tugas secara tuntas tanpa terputus (di latar belakang)"},
		{"command": "grill_me", "description": "Bot akan bertanya balik untuk memperjelas kebutuhan sebelum eksekusi"},
		{"command": "browser", "description": "Memaksa bot menggunakan browser untuk menjelajah web"},
		{"command": "schedule", "description": "Menjadwalkan atau mengatur pengingat untuk tugas"},
	}
	if err := bot.SetMyCommands(commands); err != nil {
		log.Printf("Peringatan: Gagal mengatur daftar perintah bot: %v", err)
	}

	me, err := bot.GetMe()
	if err != nil {
		log.Printf("Peringatan: Tidak dapat mengambil informasi bot (GetMe): %v", err)
	} else {
		botUsername = me.Username
		log.Printf("🤖 Username Bot: @%s", botUsername)
	}

	log.Println("Bot sedang berjalan. Tekan CTRL-C untuk keluar.")

	// Loop utama Long Polling untuk mengambil update pesan
	for {
		updates, err := bot.GetUpdates()
		if err != nil {
			log.Printf("Gagal mengambil update: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates {
			if update.Message != nil {
				go handleMessage(bot, update.Message)
			}
		}
	}
}

// getSession mengambil atau membuat sesi pengguna baru secara thread-safe
func getSession(userID int64) *SessionState {
	mu.Lock()
	defer mu.Unlock()
	state, exists := sessions[userID]
	if !exists {
		state = &SessionState{
			WorkingDir: defaultDir,
			Continue:   false,
			IsBusy:     false,
			LastActive: time.Now(),
		}
		sessions[userID] = state
	}
	return state
}

// isAuthorized memeriksa apakah pengguna atau chat diizinkan mengakses bot
func isAuthorized(userID int64, chatID int64) bool {
	// Memeriksa ID Chat jika ada konfigurasi (ChatID != UserID berarti pesan dari grup)
	if len(allowedChatIDs) > 0 && chatID != userID {
		if !allowedChatIDs[chatID] {
			return false
		}
	}

	// Jika tidak ada konfigurasi ALLOWED_USER_IDS, dianggap terbuka untuk umum
	if len(allowedUserIDs) == 0 {
		return true
	}

	if allowedUserIDs[userID] {
		return true
	}

	return false
}

// isSubDir memeriksa apakah direktori anak benar-benar berada di dalam direktori induk (mencegah path traversal)
func isSubDir(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// handleMessage memproses pesan masuk dari Telegram
func handleMessage(bot *TelegramBot, m *Message) {
	if m.From == nil || m.From.IsBot {
		return
	}

	log.Printf("📥 Menerima pesan dari User %d di Chat %d: %q", m.From.ID, m.Chat.ID, m.Text)

	if !isAuthorized(m.From.ID, m.Chat.ID) {
		log.Printf("⛔ Pesan tanpa otorisasi diabaikan dari User %d.", m.From.ID)
		return
	}

	content := strings.TrimSpace(m.Text)
	if content == "" {
		return
	}

	if botUsername != "" {
		mention := "@" + botUsername
		isGroup := m.Chat.Type == "group" || m.Chat.Type == "supergroup"

		if isGroup && !strings.HasPrefix(content, "/") && !strings.Contains(content, mention) {
			return // Abaikan pesan grup yang tidak men-tag bot atau tidak menggunakan command
		}

		// Hapus tag mention dari isi pesan agar tidak mengganggu perintah atau LLM
		content = strings.Replace(content, mention, "", -1)
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
	} else if (m.Chat.Type == "group" || m.Chat.Type == "supergroup") && !strings.HasPrefix(content, "/") {
		return // Fallback jika tidak mendapatkan username: hanya menerima perintah diawali /
	}

	// Memproses perintah Slash (/command)
	if strings.HasPrefix(content, "/") {
		parts := strings.SplitN(content, " ", 2)
		cmd := strings.ToLower(parts[0])
		args := ""
		if len(parts) > 1 {
			args = strings.TrimSpace(parts[1])
		}

		state := getSession(m.From.ID)

		switch cmd {
		case "/info", "/help":
			infoMsg := `ℹ️ <b>PANDUAN LENGKAP PERINTAH BOT</b>

Berikut adalah daftar perintah yang dapat Anda gunakan:

🔄 <b>/new</b>
• <b>Fungsi:</b> Membersihkan konteks percakapan & mulai dari awal.
• <b>Kapan Digunakan:</b> Saat ingin beralih ke topik/tugas baru tanpa terpengaruh riwayat lama.
• <b>Contoh:</b> <code>/new</code>

📂 <b>/dir &lt;path&gt;</b>
• <b>Fungsi:</b> Menampilkan, mengubah, atau membuat direktori kerja baru (Sandbox).
• <b>Batasan:</b> Hanya lokasi di dalam <code>ALLOWED_DIRS</code> yang diizinkan.
• <b>Contoh:</b> <code>/dir my-project</code> atau <code>/dir /path/to/dir</code>

🎯 <b>/goal &lt;tugas&gt;</b>
• <b>Fungsi:</b> Menjalankan tugas kompleks/panjang di latar belakang tanpa terputus.
• <b>Kapan Digunakan:</b> Cocok untuk refactoring kode, pembuatan fitur besar, atau audit.
• <b>Contoh:</b> <code>/goal Refactor seluruh kode di folder src/</code>

💬 <b>/grill_me &lt;topik&gt;</b>
• <b>Fungsi:</b> Bot akan mewawancarai Anda terlebih dahulu untuk memperjelas kebutuhan.
• <b>Kapan Digunakan:</b> Saat punya ide tapi butuh wawancara/diskusi detail sebelum eksekusi.
• <b>Contoh:</b> <code>/grill_me Desain arsitektur database e-commerce</code>

🌐 <b>/browser &lt;kueri&gt;</b>
• <b>Fungsi:</b> Memaksa agen AI menjelajah web untuk mencari informasi online.
• <b>Kapan Digunakan:</b> Untuk riset pustaka baru, dokumen API terbaru, atau solusi error.
• <b>Contoh:</b> <code>/browser Cari dokumentasi terbaru Fiber v3 Golang</code>

⏰ <b>/schedule &lt;waktu&gt; &lt;tugas&gt;</b>
• <b>Fungsi:</b> Menjadwalkan pengingat atau tugas otomatis.
• <b>Kapan Digunakan:</b> Pengingat otomatis atau tugas terjadwal.
• <b>Contoh:</b> <code>/schedule in 15m jalankan test server</code>`
			bot.SendMessage(m.Chat.ID, infoMsg, "HTML")
			return

		case "/goal", "/grill_me", "/browser":
			if args == "" {
				bot.SendMessage(m.Chat.ID, "❌ Anda perlu memberikan argumen/deskripsi untuk perintah ini.")
				return
			}
			// Mengubah /grill_me menjadi /grill-me sesuai format agy
			internalCmd := cmd
			if cmd == "/grill_me" {
				internalCmd = "/grill-me"
			}

			fullPrompt := fmt.Sprintf("%s %s", internalCmd, args)
			bot.SendMessage(m.Chat.ID, fmt.Sprintf("⏳ **[%s]** Perintah diterima, sedang diproses di latar belakang (mungkin memakan waktu beberapa menit)...", cmd))
			go executeAgy(bot, m.From.ID, m.Chat.ID, fullPrompt, true)
			return

		case "/schedule":
			if args == "" {
				bot.SendMessage(m.Chat.ID, "❌ Anda perlu memberikan waktu dan tugas (Contoh: /schedule in 5m say hello).")
				return
			}
			fullPrompt := fmt.Sprintf("/schedule %s", args)
			bot.SendMessage(m.Chat.ID, "⏳ **[/schedule]** Sedang mengatur jadwal...")
			go executeAgy(bot, m.From.ID, m.Chat.ID, fullPrompt, true)
			return

		case "/new":
			mu.Lock()
			state.Continue = false
			mu.Unlock()
			bot.SendMessage(m.Chat.ID, "🔄 Sistem siap memulai percakapan baru pada pesan berikutnya!")
			return

		case "/dir":
			if args == "" {
				bot.SendMessage(m.Chat.ID, "❌ Anda perlu memberikan jalur direktori.")
				return
			}
			reqDir := args
			mu.Lock()
			currentDir := state.WorkingDir
			mu.Unlock()

			var targetDir string
			if filepath.IsAbs(reqDir) {
				targetDir = filepath.Clean(reqDir)
			} else {
				targetDir = filepath.Clean(filepath.Join(currentDir, reqDir))
			}

			isAllowed := false
			for _, ad := range allowedDirs {
				if isSubDir(ad, targetDir) {
					isAllowed = true
					break
				}
			}

			var responseContent string
			if !isAllowed {
				responseContent = "⛔ Direktori ini tidak termasuk dalam daftar Sandbox yang diizinkan (ALLOWED_DIRS)."
			} else {
				info, err := os.Stat(targetDir)
				if os.IsNotExist(err) {
					err = os.MkdirAll(targetDir, 0755)
					if err != nil {
						responseContent = fmt.Sprintf("❌ Tidak dapat membuat direktori baru `%s`:\n%v", targetDir, err)
					} else {
						responseContent = fmt.Sprintf("📂 Berhasil membuat dan berpindah ke direktori: `%s`", targetDir)
						mu.Lock()
						state.WorkingDir = targetDir
						mu.Unlock()
					}
				} else if err != nil {
					responseContent = fmt.Sprintf("❌ Gagal membaca jalur direktori: %v", err)
				} else if !info.IsDir() {
					responseContent = fmt.Sprintf("❌ `%s` adalah sebuah file, bukan direktori!", targetDir)
				} else {
					responseContent = fmt.Sprintf("📂 Berhasil mengubah direktori kerja ke: `%s`", targetDir)
					mu.Lock()
					state.WorkingDir = targetDir
					mu.Unlock()
				}
			}
			bot.SendMessage(m.Chat.ID, responseContent)
			return
		}
	}

	// Pesan teks biasa
	bot.SendChatAction(m.Chat.ID, "typing")
	executeAgy(bot, m.From.ID, m.Chat.ID, content, false)
}

// executeAgy menjalankan proses CLI Antigravity (agy) dan mengirimkan responnya ke Telegram
func executeAgy(bot *TelegramBot, userID int64, chatID int64, content string, isBackground bool) {
	state := getSession(userID)

	cmdPath := os.Getenv("ANTIGRAVITY_CMD")
	if cmdPath == "" {
		cmdPath = "agy" // Nama binary bawaan
	}

	mu.Lock()
	// Otomatis reset sesi jika lebih dari 2 jam tidak ada interaksi (hanya berlaku untuk chat biasa)
	if !isBackground && time.Since(state.LastActive) > 2*time.Hour {
		state.Continue = false
	}
	if !isBackground {
		state.LastActive = time.Now()
	}

	if !isBackground && state.IsBusy {
		mu.Unlock()
		bot.SendMessage(chatID, "⏳ **Sistem sedang sibuk:** Bot sedang memproses permintaan Anda sebelumnya. Harap tunggu hingga selesai sebelum mengirim permintaan baru.")
		return
	}

	if !isBackground {
		state.IsBusy = true
	}

	continueFlag := state.Continue

	if isBackground {
		state.Continue = false
	}
	mu.Unlock()

	if !isBackground {
		defer func() {
			mu.Lock()
			state.IsBusy = false
			mu.Unlock()
		}()
	}

	// Menyiapkan argumen perintah untuk agy
	args := []string{
		"--sandbox",
		"--add-dir", state.WorkingDir,
		"--dangerously-skip-permissions",
		"--print-timeout", "720h", // Memperpanjang batas waktu tunggu hingga 30 hari untuk perintah /goal dan /schedule
	}

	if continueFlag {
		args = append(args, "-c")
	}

	// Menambahkan instruksi prompt sistem tersembunyi di akhir prompt
	sysPrompt := "\n\n(Catatan sistem: Dilarang menggunakan format tabel (table). Harap sajikan jawaban dalam bentuk daftar poin (bullet list) dengan cetak tebal/miring agar mudah dibaca di ponsel. Buat jawaban sesingkat dan sejelas mungkin. SANGAT PENTING: Anda WAJIB membungkus seluruh jawaban akhir untuk pengguna di dalam pasangan tag <BOT_REPLY> dan </BOT_REPLY>. Setiap pemikiran atau log internal Anda harus berada di luar tag ini agar sistem dapat menyaringnya.)"
	args = append(args, "-p", content+sysPrompt)

	log.Printf("Mengeksekusi untuk User %d di %s: %s", userID, state.WorkingDir, cmdPath)

	cmd := exec.Command(cmdPath, args...)
	cmd.Dir = state.WorkingDir

	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	// Memulai loop untuk mengirim indikator status "typing..." secara berkala
	done := make(chan bool)
	if !isBackground {
		go func() {
			for {
				select {
				case <-done:
					return
				case <-time.After(4 * time.Second):
					bot.SendChatAction(chatID, "typing")
				}
			}
		}()
	}

	err := cmd.Run()

	if !isBackground {
		done <- true // Menghentikan loop indikator typing
	}

	result := out.String()
	errStr := errOut.String()

	if errStr != "" {
		result += "\n[STDERR]\n" + errStr
	}

	// Menyaring hasil keluaran untuk mengambil isi di dalam tag <BOT_REPLY> (jika ada)
	re := regexp.MustCompile(`(?s)<BOT_REPLY>(.*?)</BOT_REPLY>`)
	matches := re.FindAllStringSubmatch(result, -1)
	if len(matches) > 0 {
		result = matches[len(matches)-1][1]
	} else if result != "" {
		// Fallback: Hapus baris-baris khas tahapan pikiran (thought process) jika model lupa mencetak tag
		lines := strings.Split(result, "\n")
		var filtered []string
		for _, line := range lines {
			lowerLine := strings.ToLower(strings.TrimSpace(line))
			if strings.HasPrefix(lowerLine, "i will ") ||
				strings.HasPrefix(lowerLine, "i am going to ") ||
				strings.HasPrefix(lowerLine, "i need to ") ||
				strings.HasPrefix(lowerLine, "let me ") {
				continue
			}
			filtered = append(filtered, line)
		}
		result = strings.Join(filtered, "\n")
	}

	result = strings.TrimSpace(result)
	if result == "" {
		if err != nil {
			result = fmt.Sprintf("Perintah selesai dengan kesalahan: %v\n\n[Raw Output]:\n%s", err, out.String())
		} else {
			result = "Perintah selesai."
		}
	}

	if err == nil && !isBackground {
		mu.Lock()
		state.Continue = true
		mu.Unlock()
	}

	sendLongMessage(bot, chatID, result)
}

// sendLongMessage memformat teks ke HTML dan memotong pesan jika melebihi batas maksimal Telegram
func sendLongMessage(bot *TelegramBot, chatID int64, text string) {
	// Format teks Markdown ke HTML Telegram
	text = formatTelegramHTML(text)

	// Batas maksimal panjang pesan Telegram adalah 4096 karakter. Gunakan 4000 untuk aman.
	const maxLen = 4000
	if utf8.RuneCountInString(text) <= maxLen {
		if err := bot.SendMessage(chatID, text, "HTML"); err != nil {
			log.Printf("Gagal mengirim pesan: %v", err)
			// Jika terjadi kesalahan parsing HTML, kirim ulang sebagai teks biasa
			bot.SendMessage(chatID, text)
		}
		return
	}

	// Memotong pesan menjadi beberapa bagian (chunks)
	runes := []rune(text)
	for i := 0; i < len(runes); i += maxLen {
		end := i + maxLen
		if end > len(runes) {
			end = len(runes)
		}
		err := bot.SendMessage(chatID, string(runes[i:end]), "HTML")
		if err != nil {
			log.Printf("Gagal mengirim potongan pesan: %v", err)
			bot.SendMessage(chatID, string(runes[i:end]))
		}
	}
}

// formatTelegramHTML mengubah format Markdown standar menjadi HTML yang kompatibel dengan Telegram
func formatTelegramHTML(text string) string {
	// 1. Meng-escape karakter < dan > untuk mencegah kesalahan parsing HTML
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")

	// 2. Mengubah blok kode ```...``` menjadi <pre>...</pre>
	rePre := regexp.MustCompile("(?s)```[a-zA-Z]*\n(.*?)\n?```")
	text = rePre.ReplaceAllString(text, "<pre>$1</pre>")

	// 3. Mengubah kode inline `...` menjadi <code>...</code>
	reCode := regexp.MustCompile("`([^`]+)`")
	text = reCode.ReplaceAllString(text, "<code>$1</code>")

	// 4. Mengubah teks tebal **...** menjadi <b>...</b>
	reBold := regexp.MustCompile(`\*\*(.+?)\*\*`)
	text = reBold.ReplaceAllString(text, "<b>$1</b>")

	// 5. Mengubah teks miring *...* menjadi <i>...</i>
	reItalic := regexp.MustCompile(`(?m)(^|[^\*])\*([^\*\s][^\*]*[^\*\s]|[^\*\s])\*([^\*]|$)`)
	text = reItalic.ReplaceAllString(text, "$1<i>$2</i>$3")

	return text
}
