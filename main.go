package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"kundalik_bot/config"
	"kundalik_bot/database"
	"kundalik_bot/scraper"

	tele "gopkg.in/telebot.v3"
)

var (
	tashkentLoc  *time.Location
	stateMutex   sync.RWMutex
	userState    = make(map[int64]string)
	userTempData = make(map[int64]map[string]string)
	isSendingNow = false
	sendMutex    sync.Mutex
)

func init() {
	var err error
	tashkentLoc, err = time.LoadLocation("Asia/Tashkent")
	if err != nil {
		tashkentLoc = time.FixedZone("Asia/Tashkent", 5*60*60)
	}
}

func setState(userID int64, state string) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	userState[userID] = state
}

func getState(userID int64) string {
	stateMutex.RLock()
	defer stateMutex.RUnlock()
	return userState[userID]
}

func setTempData(userID int64, key, val string) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	if userTempData[userID] == nil {
		userTempData[userID] = make(map[string]string)
	}
	userTempData[userID][key] = val
}

func getTempData(userID int64, key string) string {
	stateMutex.RLock()
	defer stateMutex.RUnlock()
	if userTempData[userID] == nil {
		return ""
	}
	return userTempData[userID][key]
}

func clearState(userID int64) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	delete(userState, userID)
	delete(userTempData, userID)
}

type StudentTask struct {
	ID            int
	Name          string
	Login         string
	EncryptedPass string
	ChannelID     int64
	Grade         int
	Letter        string
	FailReason    string
}

// BAHOLARNI YUBORISH (AUTO-RETRY VA STATISTIKA BILAN)
func sendAllGrades(b *tele.Bot, db *database.DB, cfg *config.Config, notifyChat *tele.Chat) {
	sendMutex.Lock()
	if isSendingNow {
		sendMutex.Unlock()
		if notifyChat != nil {
			_, _ = b.Send(notifyChat, "⚠️ Baholarni yuborish jarayoni hozir fonda ishlamoqda, kuting...")
		}
		return
	}
	isSendingNow = true
	sendMutex.Unlock()

	defer func() {
		sendMutex.Lock()
		isSendingNow = false
		sendMutex.Unlock()
	}()

	startTime := time.Now().In(tashkentLoc)

	rows, err := db.Conn.Query(`
		SELECT s.id, s.full_name, s.kundalik_login, s.kundalik_password, 
		       COALESCE(NULLIF(s.channel_id, 0), c.channel_id) as target_channel, 
		       c.grade_number, c.class_letter 
		FROM students s 
		JOIN classes c ON s.class_id = c.id
		WHERE target_channel != 0`)
	if err != nil {
		log.Println("Bazadan o'qishda xatolik:", err)
		return
	}

	var tasks []StudentTask
	for rows.Next() {
		var t StudentTask
		if err := rows.Scan(&t.ID, &t.Name, &t.Login, &t.EncryptedPass, &t.ChannelID, &t.Grade, &t.Letter); err == nil {
			tasks = append(tasks, t)
		}
	}
	rows.Close()

	total := len(tasks)
	if total == 0 {
		if notifyChat != nil {
			_, _ = b.Send(notifyChat, "⚠️ Kanali biriktirilgan birorta ham o'quvchi topilmadi. Avval sinfga kanal biriktiring!")
		}
		return
	}

	if notifyChat != nil {
		_, _ = b.Send(notifyChat, fmt.Sprintf("🚀 <b>Baholarni yuborish boshlandi!</b>\nJami: <b>%d ta o'quvchi</b>", total), tele.ModeHTML)
	}

	executeBatch := func(taskList []StudentTask) (int, []StudentTask) {
		maxConcurrency := 2
		sem := make(chan struct{}, maxConcurrency)
		var wg sync.WaitGroup

		var successCount int
		var failedList []StudentTask
		var mtx sync.Mutex

		for _, task := range taskList {
			wg.Add(1)
			sem <- struct{}{}

			go func(st StudentTask) {
				defer wg.Done()
				defer func() { <-sem }()

				defer func() {
					if r := recover(); r != nil {
						log.Printf("Xatolik [%s]: %v\n", st.Name, r)
						st.FailReason = fmt.Sprintf("Panic: %v", r)
						mtx.Lock()
						failedList = append(failedList, st)
						mtx.Unlock()
					}
				}()

				rawPass, decErr := config.Decrypt(st.EncryptedPass, cfg.SecretKey)
				if decErr != nil {
					rawPass = st.EncryptedPass
				}

				log.Printf("📸 [%s] uchun baholar olinmoqda...\n", st.Name)
				imgBytes, err := scraper.TakeGradeScreenshot(st.Login, rawPass)
				if err != nil {
					log.Printf("❌ Xato [%s]: %v\n", st.Name, err)
					st.FailReason = err.Error()
					mtx.Lock()
					failedList = append(failedList, st)
					mtx.Unlock()
					return
				}

				nowT := time.Now().In(tashkentLoc)
				caption := fmt.Sprintf(
					"👤 <b>O'quvchi:</b> %s\n"+
						"🏫 <b>Sinf:</b> %d-%s\n"+
						"📅 <b>Sana:</b> %s | %s\n"+
						"📊 <b>eMaktab.uz baholar jadvali</b>",
					st.Name, st.Grade, st.Letter,
					nowT.Format("02.01.2006"), nowT.Format("15:04"),
				)

				photo := &tele.Photo{
					File:    tele.FromReader(bytes.NewReader(imgBytes)),
					Caption: caption,
				}

				_, err = b.Send(&tele.Chat{ID: st.ChannelID}, photo, tele.ModeHTML)
				if err != nil {
					log.Printf("❌ Kanalga yuborishda xato [%s]: %v\n", st.Name, err)
					st.FailReason = "Kanalga yuborib bo'lmadi (Bot kanalda admin emas)"
					mtx.Lock()
					failedList = append(failedList, st)
					mtx.Unlock()
				} else {
					mtx.Lock()
					successCount++
					mtx.Unlock()
				}

				time.Sleep(time.Duration(1500+rand.Intn(1000)) * time.Millisecond)
			}(task)
		}

		wg.Wait()
		return successCount, failedList
	}

	success1, failed1 := executeBatch(tasks)

	successRetry := 0
	var finalFailed []StudentTask
	if len(failed1) > 0 {
		if notifyChat != nil {
			_, _ = b.Send(notifyChat, fmt.Sprintf("🔄 <b>Auto-Retry:</b> %d ta o'quvchi 15 soniyadan so'ng qayta tekshiriladi...", len(failed1)), tele.ModeHTML)
		}
		time.Sleep(15 * time.Second)
		successRetry, finalFailed = executeBatch(failed1)
	}

	totalSuccess := success1 + successRetry
	elapsed := time.Since(startTime).Round(time.Second)

	if notifyChat != nil {
		var report strings.Builder
		report.WriteString("📊 <b>Baholar yuborish yakunlandi:</b>\n\n")
		report.WriteString(fmt.Sprintf("👥 Jami o'quvchilar: <b>%d ta</b>\n", total))
		report.WriteString(fmt.Sprintf("✅ Yuborildi: <b>%d ta</b>\n", totalSuccess))
		report.WriteString(fmt.Sprintf("⏱ Vaqt: <b>%s</b>\n", elapsed))

		if len(finalFailed) > 0 {
			report.WriteString(fmt.Sprintf("\n❌ <b>Yuborilmaganlar (%d ta):</b>\n", len(finalFailed)))
			for i, f := range finalFailed {
				report.WriteString(fmt.Sprintf("%d. <b>%s</b> (%d-%s)\n   Sabab: <i>%s</i>\n", i+1, f.Name, f.Grade, f.Letter, f.FailReason))
			}
		} else {
			report.WriteString("\n🎉 Barcha o'quvchilarning baholari yuborildi!")
		}
		_, _ = b.Send(notifyChat, report.String(), tele.ModeHTML)
	}
}

// DARS JADVALINI SINF KANALIGA YUBORISH
func sendScheduleToClassChannel(b *tele.Bot, db *database.DB, classID int, dayName string) (bool, string) {
	var grade int
	var letter string
	var channelID int64
	err := db.Conn.QueryRow("SELECT grade_number, class_letter, channel_id FROM classes WHERE id = ?", classID).Scan(&grade, &letter, &channelID)
	if err != nil || channelID == 0 {
		return false, "⚠️ Ushbu sinfga hali kanal biriktirilmagan!"
	}

	var lessons string
	err = db.Conn.QueryRow("SELECT lessons FROM schedules WHERE class_id = ? AND day_of_week = ?", classID, dayName).Scan(&lessons)
	if err != nil || strings.TrimSpace(lessons) == "" {
		return false, fmt.Sprintf("⚠️ <b>%s</b> kuni uchun dars jadvali kiritilmagan.", dayName)
	}

	msg := fmt.Sprintf("📅 <b>Dars jadvali (%s):</b>\n🏫 Sinf: <b>%d-%s</b>\n────────────────────\n%s",
		dayName, grade, letter, lessons)

	_, err = b.Send(&tele.Chat{ID: channelID}, msg, tele.ModeHTML)
	if err != nil {
		return false, fmt.Sprintf("❌ Kanalga yuborishda xatolik: %v (Bot kanalda admin ekanini tekshiring)", err)
	}

	return true, fmt.Sprintf("✅ <b>%d-%s</b> sinfining <b>%s</b> kungi dars jadvali kanalga yuborildi!", grade, letter, dayName)
}

// HAR KUNI AVTOMATIK DARS JADVALLARINI KANALLARGA YUBORISH
func sendDailySchedules(b *tele.Bot, db *database.DB, notifyChat *tele.Chat) {
	tomorrow := time.Now().In(tashkentLoc).AddDate(0, 0, 1)
	dayName := ""
	switch tomorrow.Weekday() {
	case time.Monday:
		dayName = "Dushanba"
	case time.Tuesday:
		dayName = "Seshanba"
	case time.Wednesday:
		dayName = "Chorshanba"
	case time.Thursday:
		dayName = "Payshanba"
	case time.Friday:
		dayName = "Juma"
	case time.Saturday:
		dayName = "Shanba"
	case time.Sunday:
		return
	}

	rows, err := db.Conn.Query(`
		SELECT c.id, c.grade_number, c.class_letter, c.channel_id, s.lessons 
		FROM schedules s
		JOIN classes c ON s.class_id = c.id
		WHERE s.day_of_week = ? AND c.channel_id != 0`, dayName)
	if err != nil {
		return
	}
	defer rows.Close()

	sentCount := 0
	for rows.Next() {
		var classID, grade int
		var letter, lessons string
		var channelID int64
		if err := rows.Scan(&classID, &grade, &letter, &channelID, &lessons); err == nil {
			msg := fmt.Sprintf("📅 <b>Ertangi kun dars jadvali (%s):</b>\n🏫 Sinf: <b>%d-%s</b>\n────────────────────\n%s",
				dayName, grade, letter, lessons)
			_, errSend := b.Send(&tele.Chat{ID: channelID}, msg, tele.ModeHTML)
			if errSend == nil {
				sentCount++
			}
			time.Sleep(2 * time.Second)
		}
	}

	if notifyChat != nil && sentCount > 0 {
		_, _ = b.Send(notifyChat, fmt.Sprintf("✅ <b>%s</b> kunlik dars jadvallari %d ta sinf kanaliga yuborildi.", dayName, sentCount), tele.ModeHTML)
	}
}

// CRON SCHEDULER
func startScheduler(b *tele.Bot, db *database.DB, cfg *config.Config) {
	go func() {
		lastGradeDay := -1
		lastSchedDay := -1

		for {
			now := time.Now().In(tashkentLoc)
			currentHM := now.Format("15:04")

			var gradeTime string
			_ = db.Conn.QueryRow("SELECT value FROM settings WHERE key = 'send_time'").Scan(&gradeTime)
			if gradeTime != "" && currentHM == gradeTime && now.Day() != lastGradeDay {
				lastGradeDay = now.Day()
				ownerChat := &tele.Chat{ID: cfg.OwnerID}
				_, _ = b.Send(ownerChat, fmt.Sprintf("⏰ <b>Baholarni avto-yuborish vaqti (%s) bo'ldi.</b>", gradeTime), tele.ModeHTML)
				sendAllGrades(b, db, cfg, ownerChat)
			}

			var schedTime string
			_ = db.Conn.QueryRow("SELECT value FROM settings WHERE key = 'schedule_time'").Scan(&schedTime)
			if schedTime != "" && currentHM == schedTime && now.Day() != lastSchedDay {
				lastSchedDay = now.Day()
				ownerChat := &tele.Chat{ID: cfg.OwnerID}
				sendDailySchedules(b, db, ownerChat)
			}

			time.Sleep(30 * time.Second)
		}
	}()
}

func main() {
	cfg := config.LoadConfig()
	db := database.InitDB()
	defer db.Conn.Close()

	_, _ = db.Conn.Exec("ALTER TABLE students ADD COLUMN channel_id BIGINT DEFAULT 0")

	b, err := tele.NewBot(tele.Settings{
		Token:  cfg.BotToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	})
	if err != nil {
		log.Fatal("Bot xatosi:", err)
	}

	startScheduler(b, db, cfg)
	// Render bergan PORT ni olish
port := os.Getenv("PORT")
if port == "" {
    port = "8080"
}

// Render uxlamasligi uchun ping yo'li
http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("Bot faol!"))
})

go func() {
    log.Println("HTTP server ishga tushdi:", port)
    if err := http.ListenAndServe(":"+port, nil); err != nil {
        log.Println("Server xatosi:", err)
    }
}()

// Agar Render domenini o'ziga har 9 daqiqada ping qildirmoqchi bo'lsangiz:
appURL := os.Getenv("APP_URL")
if appURL != "" {
    go func() {
        ticker := time.NewTicker(9 * time.Minute)
        for range ticker.C {
            resp, err := http.Get(appURL + "/ping")
            if err == nil {
                resp.Body.Close()
            }
        }
    }()
}

	isOwnerOrAdmin := func(userID int64) (bool, bool) {
		if userID == cfg.OwnerID {
			return true, true
		}
		var exists bool
		_ = db.Conn.QueryRow("SELECT EXISTS(SELECT 1 FROM admins WHERE telegram_id = ?)", userID).Scan(&exists)
		return false, exists
	}

	// ==================== ASOSIY MENYU (/START) ====================
    b.Handle("/start", func(c tele.Context) error {
        userID := c.Sender().ID
        isOwner, isAdmin := isOwnerOrAdmin(userID)

        // Begona odam bo'lsa, hech qanday javob qaytarmaydi
        if !isOwner && !isAdmin {
            return nil
        }

        clearState(userID)
		menu := &tele.ReplyMarkup{ResizeKeyboard: true}

		btnSchool := menu.Text("🏫 Maktab va Sinflar")
		btnSend := menu.Text("⚡️ Tezkor yuborish")
		btnAdd := menu.Text("➕ Ma'lumot qo'shish")
		btnSettings := menu.Text("⚙️ Sozlamalar")

		menu.Reply(
			menu.Row(btnSchool, btnSend),
			menu.Row(btnAdd, btnSettings),
		)

		role := "Admin"
		if isOwner {
			role = "👑 Boshqaruvchi (Owner)"
		}

		welcomeText := fmt.Sprintf("Assalomu alaykum, <b>%s</b>!\n\nKerakli bo'limni tanlang:", role)
		return c.Send(welcomeText, menu, tele.ModeHTML)
	})

	// 1. MAKTAB VA SINFLAR
	b.Handle("🏫 Maktab va Sinflar", func(c tele.Context) error {
		inline := &tele.ReplyMarkup{}
		btnClasses := inline.Data("📚 Sinflar & O'quvchilar", "nav_classes")
		btnSchedules := inline.Data("📅 Dars jadvallari", "nav_schedules")
		btnChannels := inline.Data("📑 Kanallar ro'yxati", "nav_channels")

		inline.Inline(
			inline.Row(btnClasses),
			inline.Row(btnSchedules),
			inline.Row(btnChannels),
		)
		return c.Send("🏫 <b>Maktab va Sinflar bo'limi:</b>", inline, tele.ModeHTML)
	})

	// 2. MA'LUMOT QO'SHISH
	b.Handle("➕ Ma'lumot qo'shish", func(c tele.Context) error {
		inline := &tele.ReplyMarkup{}
		btnAddStudent := inline.Data("👤 O'quvchi qo'shish", "nav_add_student")
		btnAddChannel := inline.Data("📢 Yangi kanal ulash", "nav_add_channel")
		btnAddAdmin := inline.Data("👥 Yangi admin tayinlash", "nav_add_admin")

		inline.Inline(
			inline.Row(btnAddStudent),
			inline.Row(btnAddChannel),
			inline.Row(btnAddAdmin),
		)
		return c.Send("➕ <b>Yangi ma'lumot qo'shish:</b>", inline, tele.ModeHTML)
	})

	// 3. TEZKOR YUBORISH
	b.Handle("⚡️ Tezkor yuborish", func(c tele.Context) error {
		inline := &tele.ReplyMarkup{}
		btnSendGrades := inline.Data("📊 Baholarni hozir yuborish", "nav_send_grades")
		btnSendScheds := inline.Data("📅 Ertangi dars jadvallarini yuborish", "nav_send_scheds")

		inline.Inline(
			inline.Row(btnSendGrades),
			inline.Row(btnSendScheds),
		)
		return c.Send("⚡️ <b>Tezkor yuborish bo'limi:</b>", inline, tele.ModeHTML)
	})

	// 4. SOZLAMALAR
	b.Handle("⚙️ Sozlamalar", func(c tele.Context) error {
		isOwner, _ := isOwnerOrAdmin(c.Sender().ID)
		if !isOwner {
			return c.Send("⛔ Sozlamalar bo'limi faqat Owner uchun ochiq.")
		}

		inline := &tele.ReplyMarkup{}
		btnSetTime := inline.Data("⏰ Avto-vaqtlarni sozlash", "nav_set_time")
		btnAdmins := inline.Data("👥 Adminlarni boshqarish", "nav_manage_admins")
		btnBackup := inline.Data("💾 Baza zaxira nusxasi (Backup)", "nav_backup")

		inline.Inline(
			inline.Row(btnSetTime),
			inline.Row(btnAdmins),
			inline.Row(btnBackup),
		)
		return c.Send("⚙️ <b>Tizim sozlamalari:</b>", inline, tele.ModeHTML)
	})

	// ==================== NAVIGATSIYA CALLBACKLARI ====================
	b.Handle(&tele.Btn{Unique: "nav_classes"}, func(c tele.Context) error {
		inlineMenu := &tele.ReplyMarkup{}
		var rowsBtn []tele.Row
		for g := 1; g <= 11; g++ {
			btnA := inlineMenu.Data(fmt.Sprintf("%d-A", g), "cls_view", fmt.Sprintf("%d_A", g))
			btnB := inlineMenu.Data(fmt.Sprintf("%d-B", g), "cls_view", fmt.Sprintf("%d_B", g))
			rowsBtn = append(rowsBtn, inlineMenu.Row(btnA, btnB))
		}
		inlineMenu.Inline(rowsBtn...)
		_ = c.Respond()
		return c.Edit("📋 Kerakli sinfni tanlang:", inlineMenu)
	})

	b.Handle(&tele.Btn{Unique: "nav_schedules"}, func(c tele.Context) error {
		inlineMenu := &tele.ReplyMarkup{}
		var rowsBtn []tele.Row
		for g := 1; g <= 11; g++ {
			btnA := inlineMenu.Data(fmt.Sprintf("%d-A", g), "sch_cls", fmt.Sprintf("%d_A", g))
			btnB := inlineMenu.Data(fmt.Sprintf("%d-B", g), "sch_cls", fmt.Sprintf("%d_B", g))
			rowsBtn = append(rowsBtn, inlineMenu.Row(btnA, btnB))
		}
		inlineMenu.Inline(rowsBtn...)
		_ = c.Respond()
		return c.Edit("📅 Dars jadvalini ko'rish yoki tahrirlash uchun sinfni tanlang:", inlineMenu)
	})

	b.Handle(&tele.Btn{Unique: "nav_channels"}, func(c tele.Context) error {
		rows, err := db.Conn.Query("SELECT channel_id, title FROM channels")
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Baza xatosi."})
		}
		defer rows.Close()

		var msg strings.Builder
		msg.WriteString("📑 <b>Ulangan kanallar ro'yxati:</b>\n\n")
		count := 0
		for rows.Next() {
			var chID int64
			var title string
			_ = rows.Scan(&chID, &title)
			count++
			msg.WriteString(fmt.Sprintf("%d. 📢 <b>%s</b> (ID: <code>%d</code>)\n", count, title, chID))
		}
		if count == 0 {
			msg.WriteString("<i>Hozircha birorta ham kanal ulanmagan.</i>")
		}

		_ = c.Respond()
		return c.Edit(msg.String(), tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "nav_add_student"}, func(c tele.Context) error {
		var chCount int
		_ = db.Conn.QueryRow("SELECT COUNT(*) FROM channels").Scan(&chCount)
		if chCount == 0 {
			_ = c.Respond(&tele.CallbackResponse{Text: "Avval kanal qo'shing!"})
			return c.Send("⚠️ Avval tizimga kamida bitta kanal qo'shing!", tele.ModeHTML)
		}
		setState(c.Sender().ID, "STUDENT_ENTER_NAME")
		_ = c.Respond()
		return c.Send("O'quvchining <b>Ism va Familiyasi</b>ni yozing:", tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "nav_add_channel"}, func(c tele.Context) error {
		setState(c.Sender().ID, "CHANNEL_ENTER_ID")
		_ = c.Respond()
		return c.Send("Kanal yoki guruh ID sini yuboring (masalan: <code>-1001234567890</code>):\n\n<i>Eslatma: Bot kanalda admin bo'lishi kerak!</i>", tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "nav_add_admin"}, func(c tele.Context) error {
		if c.Sender().ID != cfg.OwnerID {
			return c.Respond(&tele.CallbackResponse{Text: "Faqat Owner admin qo'sha oladi!"})
		}
		setState(c.Sender().ID, "ADMIN_ENTER_ID")
		_ = c.Respond()
		return c.Send("Yangi adminning <b>Telegram ID</b> sini yuboring:", tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "nav_send_grades"}, func(c tele.Context) error {
		_ = c.Respond(&tele.CallbackResponse{Text: "Yuborish boshlandi!"})
		go sendAllGrades(b, db, cfg, c.Chat())
		return nil
	})

	b.Handle(&tele.Btn{Unique: "nav_send_scheds"}, func(c tele.Context) error {
		_ = c.Respond(&tele.CallbackResponse{Text: "Jadvallar yuborilmoqda..."})
		go sendDailySchedules(b, db, c.Chat())
		return nil
	})

	b.Handle(&tele.Btn{Unique: "nav_set_time"}, func(c tele.Context) error {
		var gradeTime, schedTime string
		_ = db.Conn.QueryRow("SELECT value FROM settings WHERE key = 'send_time'").Scan(&gradeTime)
		_ = db.Conn.QueryRow("SELECT value FROM settings WHERE key = 'schedule_time'").Scan(&schedTime)

		inline := &tele.ReplyMarkup{}
		btnSetGrade := inline.Data("📊 Baholar vaqti", "set_time_type", "grade")
		btnSetSched := inline.Data("📅 Dars jadvali vaqti", "set_time_type", "schedule")
		inline.Inline(inline.Row(btnSetGrade, btnSetSched))

		msg := fmt.Sprintf("⏰ <b>Avtomatik vaqtlar (Asia/Tashkent):</b>\n\n📊 Baholar yuborish: <b>%s</b>\n📅 Dars jadvali yuborish: <b>%s</b>\n\nQaysi birini o'zgartirmoqchisiz?", gradeTime, schedTime)
		_ = c.Respond()
		return c.Edit(msg, inline, tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "nav_manage_admins"}, func(c tele.Context) error {
		rows, err := db.Conn.Query("SELECT telegram_id, full_name FROM admins")
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Xatolik yuz berdi."})
		}
		defer rows.Close()

		var msg strings.Builder
		msg.WriteString("👥 <b>Adminlar ro'yxati:</b>\n\n")
		count := 0
		for rows.Next() {
			var tgID int64
			var name string
			_ = rows.Scan(&tgID, &name)
			count++
			msg.WriteString(fmt.Sprintf("%d. <b>%s</b> | ID: <code>%d</code>\n", count, name, tgID))
		}
		if count == 0 {
			msg.WriteString("<i>Hozircha adminlar tayinlanmagan.</i>\n")
		}

		inline := &tele.ReplyMarkup{}
		btnDelAdmin := inline.Data("➖ Adminni o'chirish", "call_del_admin")
		inline.Inline(inline.Row(btnDelAdmin))

		_ = c.Respond()
		return c.Edit(msg.String(), inline, tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "call_del_admin"}, func(c tele.Context) error {
		setState(c.Sender().ID, "WAITING_DEL_ADMIN")
		_ = c.Respond()
		return c.Send("O'chirmoqchi bo'lgan adminning <b>Telegram ID</b> sini yuboring:", tele.ModeHTML)
	})

	// XAVFSIZ SQLITE HOT-BACKUP (VACUUM INTO)
	b.Handle(&tele.Btn{Unique: "nav_backup"}, func(c tele.Context) error {
		nowT := time.Now().In(tashkentLoc)
		backupFile := fmt.Sprintf("./backup_%s.db", nowT.Format("20060102_150405"))
		_ = os.Remove(backupFile)

		_, err := db.Conn.Exec(fmt.Sprintf("VACUUM INTO '%s'", backupFile))
		if err != nil {
			_ = c.Respond(&tele.CallbackResponse{Text: "Backup olishda xato!"})
			return c.Send(fmt.Sprintf("❌ Backup olishda xatolik: %v", err))
		}
		defer os.Remove(backupFile)

		doc := &tele.Document{
			File:     tele.FromDisk(backupFile),
			FileName: fmt.Sprintf("backup_bot_%s.db", nowT.Format("2006-01-02_15-04")),
			Caption:  "💾 <b>Ma'lumotlar bazasining yaxlit zaxira nusxasi (WAL toza olingan).</b>",
		}
		_ = c.Respond()
		return c.Send(doc, tele.ModeHTML)
	})

	// ==================== TARTIBLI SINF KO'RINISHI (RAQAMLI TUGMALAR) ====================
	renderClassView := func(c tele.Context, grade int, letter string) error {
		var classID int
		var channelID int64
		_ = db.Conn.QueryRow("SELECT id, channel_id FROM classes WHERE grade_number = ? AND class_letter = ?", grade, letter).Scan(&classID, &channelID)

		channelTitle := "Ulangan kanal yo'q ❌"
		if channelID != 0 {
			var t string
			err := db.Conn.QueryRow("SELECT title FROM channels WHERE channel_id = ?", channelID).Scan(&t)
			if err == nil && t != "" {
				channelTitle = fmt.Sprintf("📢 %s", t)
			} else {
				channelTitle = fmt.Sprintf("ID: <code>%d</code>", channelID)
			}
		}

		rows, _ := db.Conn.Query("SELECT id, full_name, kundalik_login FROM students WHERE class_id = ? ORDER BY full_name ASC", classID)
		defer rows.Close()

		var msg strings.Builder
		msg.WriteString(fmt.Sprintf("🏫 <b>Sinf: %d-%s</b>\n", grade, letter))
		msg.WriteString(fmt.Sprintf("📡 <b>Biriktirilgan kanal:</b> %s\n", channelTitle))
		msg.WriteString("────────────────────\n")
		msg.WriteString("👨‍🎓 <b>O'quvchilar ro'yxati:</b>\n\n")

		inlineMenu := &tele.ReplyMarkup{}
		var numberBtns []tele.Btn
		var numberRows []tele.Row

		count := 0
		for rows.Next() {
			var sID int
			var sName, sLogin string
			_ = rows.Scan(&sID, &sName, &sLogin)
			count++
			msg.WriteString(fmt.Sprintf("<b>%d.</b> %s (<code>%s</code>)\n", count, sName, sLogin))

			// Har bir o'quvchi uchun ixcham raqamli tugma
			btnNum := inlineMenu.Data(fmt.Sprintf("👤 %d", count), "st_card", fmt.Sprintf("%d_%d_%s", sID, grade, letter))
			numberBtns = append(numberBtns, btnNum)

			// Bir qatorda 5 tagacha raqam tugmasi
			if len(numberBtns) == 5 {
				numberRows = append(numberRows, inlineMenu.Row(numberBtns...))
				numberBtns = []tele.Btn{}
			}
		}
		if len(numberBtns) > 0 {
			numberRows = append(numberRows, inlineMenu.Row(numberBtns...))
		}

		if count == 0 {
			msg.WriteString("<i>Bu sinfda hali o'quvchilar yo'q.</i>\n")
		} else {
			msg.WriteString("\n<i>ℹ️ O'quvchini boshqarish yoki o'chirish uchun pastdagi raqamini bosing:</i>\n")
		}

		btnBind := inlineMenu.Data("📢 Sinfga kanal ulash / o'zgartirish", "bind_ch_cls", strconv.Itoa(classID))
		btnSched := inlineMenu.Data("📅 Dars jadvalini ko'rish", "sch_cls", fmt.Sprintf("%d_%s", grade, letter))
		btnBack := inlineMenu.Data("⬅️ Sinflarga qaytish", "nav_classes")

		var finalRows []tele.Row
		finalRows = append(finalRows, inlineMenu.Row(btnBind))
		finalRows = append(finalRows, inlineMenu.Row(btnSched))
		finalRows = append(finalRows, numberRows...)
		finalRows = append(finalRows, inlineMenu.Row(btnBack))

		inlineMenu.Inline(finalRows...)
		return c.Edit(msg.String(), inlineMenu, tele.ModeHTML)
	}

	b.Handle(&tele.Btn{Unique: "cls_view"}, func(c tele.Context) error {
		parts := strings.Split(c.Data(), "_")
		grade, _ := strconv.Atoi(parts[0])
		letter := parts[1]
		_ = c.Respond()
		return renderClassView(c, grade, letter)
	})

	// ==================== ALOHIDA O'QUVCHI KARTASI / PROFILI ====================
	b.Handle(&tele.Btn{Unique: "st_card"}, func(c tele.Context) error {
		parts := strings.Split(c.Data(), "_")
		stID, _ := strconv.Atoi(parts[0])
		grade, _ := strconv.Atoi(parts[1])
		letter := parts[2]

		var sName, sLogin string
		var classID int
		var channelID int64
		err := db.Conn.QueryRow(`
			SELECT s.full_name, s.kundalik_login, s.class_id, COALESCE(NULLIF(s.channel_id, 0), c.channel_id) 
			FROM students s 
			JOIN classes c ON s.class_id = c.id 
			WHERE s.id = ?`, stID).Scan(&sName, &sLogin, &classID, &channelID)

		if err != nil {
			_ = c.Respond(&tele.CallbackResponse{Text: "O'quvchi topilmadi!"})
			return renderClassView(c, grade, letter)
		}

		chName := "Biriktirilmagan ❌"
		if channelID != 0 {
			var t string
			_ = db.Conn.QueryRow("SELECT title FROM channels WHERE channel_id = ?", channelID).Scan(&t)
			if t != "" {
				chName = fmt.Sprintf("📢 %s", t)
			}
		}

		cardMsg := fmt.Sprintf(
			"👤 <b>O'quvchi kartasi:</b>\n\n"+
				"🏷 <b>Ism-Familiya:</b> %s\n"+
				"🏫 <b>Sinf:</b> %d-%s\n"+
				"🔑 <b>Kundalik Login:</b> <code>%s</code>\n"+
				"🔒 <b>Parol:</b> <i>(AES-256 bilan shifrlangan)</i>\n"+
				"📡 <b>Kanal:</b> %s\n\n"+
				"<i>Quyidagi tugmalar orqali o'quvchini boshqarishingiz mumkin:</i>",
			sName, grade, letter, sLogin, chName,
		)

		inline := &tele.ReplyMarkup{}
		btnTestGrade := inline.Data("📸 Bahosini hozir tekshirish (Test)", "test_st_grade", fmt.Sprintf("%d_%d_%s", stID, grade, letter))
		btnDel := inline.Data("🗑 O'quvchini o'chirish", "del_st", fmt.Sprintf("%d_%d_%s", stID, grade, letter))
		btnBack := inline.Data("⬅️ Ro'yxatga qaytish", "cls_view", fmt.Sprintf("%d_%s", grade, letter))

		inline.Inline(
			inline.Row(btnTestGrade),
			inline.Row(btnDel),
			inline.Row(btnBack),
		)

		_ = c.Respond()
		return c.Edit(cardMsg, inline, tele.ModeHTML)
	})

	// FAQAT SHU O'QUVCHINING BAHOSINI TEKSHIRIB KO'RISH (TEST)
	b.Handle(&tele.Btn{Unique: "test_st_grade"}, func(c tele.Context) error {
		parts := strings.Split(c.Data(), "_")
		stID, _ := strconv.Atoi(parts[0])

		var name, login, encPass string
		var grade int
		var letter string
		err := db.Conn.QueryRow(`
			SELECT s.full_name, s.kundalik_login, s.kundalik_password, c.grade_number, c.class_letter 
			FROM students s 
			JOIN classes c ON s.class_id = c.id 
			WHERE s.id = ?`, stID).Scan(&name, &login, &encPass, &grade, &letter)

		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "O'quvchi topilmadi!"})
		}

		_ = c.Respond(&tele.CallbackResponse{Text: "Skrinshot olinmoqda, kuting..."})
		_ = c.Send(fmt.Sprintf("⏳ <b>%s</b> uchun eMaktab.uz tizimidan skrinshot olinmoqda...", name), tele.ModeHTML)

		go func() {
			rawPass, decErr := config.Decrypt(encPass, cfg.SecretKey)
			if decErr != nil {
				rawPass = encPass
			}

			imgBytes, err := scraper.TakeGradeScreenshot(login, rawPass)
			if err != nil {
				_, _ = b.Send(c.Chat(), fmt.Sprintf("❌ <b>Xatolik yuz berdi:</b> %v\n\nLogin yoki parol noto'g'ri bo'lishi mumkin.", err), tele.ModeHTML)
				return
			}

			photo := &tele.Photo{
				File:    tele.FromReader(bytes.NewReader(imgBytes)),
				Caption: fmt.Sprintf("✅ <b>Test muvaffaqiyatli!</b>\n👤 O'quvchi: %s (%d-%s)\n🔑 Login: <code>%s</code>", name, grade, letter, login),
			}
			_, _ = b.Send(c.Chat(), photo, tele.ModeHTML)
		}()

		return nil
	})

	// O'QUVCHINI O'CHIRISH
	b.Handle(&tele.Btn{Unique: "del_st"}, func(c tele.Context) error {
		parts := strings.Split(c.Data(), "_")
		stID, _ := strconv.Atoi(parts[0])
		grade, _ := strconv.Atoi(parts[1])
		letter := parts[2]

		_, _ = db.Conn.Exec("DELETE FROM students WHERE id = ?", stID)
		_ = c.Respond(&tele.CallbackResponse{Text: "O'quvchi muvaffaqiyatli o'chirildi!"})

		return renderClassView(c, grade, letter)
	})

	b.Handle(&tele.Btn{Unique: "bind_ch_cls"}, func(c tele.Context) error {
		classIDStr := c.Data()
		classID, _ := strconv.Atoi(classIDStr)

		rows, err := db.Conn.Query("SELECT channel_id, title FROM channels")
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Baza xatosi!"})
		}
		defer rows.Close()

		inline := &tele.ReplyMarkup{}
		var rowsBtn []tele.Row
		for rows.Next() {
			var chID int64
			var title string
			_ = rows.Scan(&chID, &title)
			btn := inline.Data(fmt.Sprintf("📢 %s", title), "set_ch_to_cls", fmt.Sprintf("%d_%d", classID, chID))
			rowsBtn = append(rowsBtn, inline.Row(btn))
		}

		if len(rowsBtn) == 0 {
			_ = c.Respond(&tele.CallbackResponse{Text: "Avval bitta kanal qo'shing!"})
			return c.Send("⚠️ Hali tizimga kanallar qo'shilmagan. Avval '➕ Ma'lumot qo'shish' bo'limidan kanal qo'shing.")
		}

		btnCancel := inline.Data("❌ Bekor qilish", "cancel_bind", strconv.Itoa(classID))
		rowsBtn = append(rowsBtn, inline.Row(btnCancel))
		inline.Inline(rowsBtn...)

		_ = c.Respond()
		return c.Edit("Ushbu sinfga biriktirmoqchi bo'lgan kanalni tanlang:", inline)
	})

	b.Handle(&tele.Btn{Unique: "set_ch_to_cls"}, func(c tele.Context) error {
		parts := strings.Split(c.Data(), "_")
		classID, _ := strconv.Atoi(parts[0])
		channelID, _ := strconv.ParseInt(parts[1], 10, 64)

		_, err := db.Conn.Exec("UPDATE classes SET channel_id = ? WHERE id = ?", channelID, classID)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Xatolik yuz berdi!"})
		}

		var grade int
		var letter string
		_ = db.Conn.QueryRow("SELECT grade_number, class_letter FROM classes WHERE id = ?", classID).Scan(&grade, &letter)

		var channelTitle string
		_ = db.Conn.QueryRow("SELECT title FROM channels WHERE channel_id = ?", channelID).Scan(&channelTitle)

		_ = c.Respond(&tele.CallbackResponse{Text: "Kanal biriktirildi!"})

		inline := &tele.ReplyMarkup{}
		btnBack := inline.Data("⬅️ Sinfga qaytish", "cls_view", fmt.Sprintf("%d_%s", grade, letter))
		inline.Inline(inline.Row(btnBack))

		return c.Edit(fmt.Sprintf("✅ <b>%d-%s sinfiga kanal biriktirildi!</b>\n\n📢 Kanal: <b>%s</b>\nID: <code>%d</code>",
			grade, letter, channelTitle, channelID), inline, tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "cancel_bind"}, func(c tele.Context) error {
		_ = c.Respond()
		return c.Edit("Bekor qilindi.")
	})

	b.Handle(&tele.Btn{Unique: "sch_cls"}, func(c tele.Context) error {
		classKey := c.Data()
		parts := strings.Split(classKey, "_")
		grade, _ := strconv.Atoi(parts[0])
		letter := parts[1]

		var classID int
		var channelID int64
		_ = db.Conn.QueryRow("SELECT id, channel_id FROM classes WHERE grade_number = ? AND class_letter = ?", grade, letter).Scan(&classID, &channelID)

		channelStatus := "Biriktirilmagan ❌"
		if channelID != 0 {
			var t string
			_ = db.Conn.QueryRow("SELECT title FROM channels WHERE channel_id = ?", channelID).Scan(&t)
			channelStatus = fmt.Sprintf("📢 %s", t)
		}

		days := []string{"Dushanba", "Seshanba", "Chorshanba", "Payshanba", "Juma", "Shanba"}
		inline := &tele.ReplyMarkup{}
		var rowsBtn []tele.Row

		for i := 0; i < len(days); i += 2 {
			btn1 := inline.Data(days[i], "sch_day", fmt.Sprintf("%d_%s", classID, days[i]))
			if i+1 < len(days) {
				btn2 := inline.Data(days[i+1], "sch_day", fmt.Sprintf("%d_%s", classID, days[i+1]))
				rowsBtn = append(rowsBtn, inline.Row(btn1, btn2))
			} else {
				rowsBtn = append(rowsBtn, inline.Row(btn1))
			}
		}

		btnBindCh := inline.Data("📢 Kanalni sozlash", "bind_ch_cls", strconv.Itoa(classID))
		btnBack := inline.Data("⬅️ Sinflarga qaytish", "nav_schedules")
		rowsBtn = append(rowsBtn, inline.Row(btnBindCh), inline.Row(btnBack))

		inline.Inline(rowsBtn...)
		_ = c.Respond()
		return c.Edit(fmt.Sprintf("🏫 <b>%d-%s sinf</b> dars jadvali.\n📡 Kanal: <b>%s</b>\n\nKerakli kunni tanlang:", grade, letter, channelStatus), inline, tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "sch_day"}, func(c tele.Context) error {
		data := c.Data()
		parts := strings.Split(data, "_")
		classID, _ := strconv.Atoi(parts[0])
		dayName := parts[1]

		var grade int
		var letter string
		var channelID int64
		_ = db.Conn.QueryRow("SELECT grade_number, class_letter, channel_id FROM classes WHERE id = ?", classID).Scan(&grade, &letter, &channelID)

		var currentLessons string
		err := db.Conn.QueryRow("SELECT lessons FROM schedules WHERE class_id = ? AND day_of_week = ?", classID, dayName).Scan(&currentLessons)
		if err != nil || currentLessons == "" {
			currentLessons = "<i>(Ushbu kunga dars jadvali kiritilmagan)</i>"
		}

		channelInfo := "Ulangan kanal yo'q ❌"
		if channelID != 0 {
			var ct string
			_ = db.Conn.QueryRow("SELECT title FROM channels WHERE channel_id = ?", channelID).Scan(&ct)
			channelInfo = fmt.Sprintf("📢 <b>%s</b>", ct)
		}

		msg := fmt.Sprintf("🏫 <b>Sinf:</b> %d-%s\n📅 <b>Kun:</b> %s\n📡 <b>Kanal:</b> %s\n────────────────────\n📖 <b>Darslar:</b>\n%s",
			grade, letter, dayName, channelInfo, currentLessons)

		inline := &tele.ReplyMarkup{}
		btnEdit := inline.Data("✏️ Tahrirlash / Yozish", "edit_sch", fmt.Sprintf("%d_%s", classID, dayName))
		btnSendChannel := inline.Data("🚀 Kanalga hoziroq yuborish", "send_sch_now", fmt.Sprintf("%d_%s", classID, dayName))
		btnBack := inline.Data("⬅️ Kunlarga qaytish", "sch_cls", fmt.Sprintf("%d_%s", grade, letter))

		inline.Inline(
			inline.Row(btnEdit),
			inline.Row(btnSendChannel),
			inline.Row(btnBack),
		)

		_ = c.Respond()
		return c.Edit(msg, inline, tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "send_sch_now"}, func(c tele.Context) error {
		parts := strings.Split(c.Data(), "_")
		classID, _ := strconv.Atoi(parts[0])
		dayName := parts[1]

		success, text := sendScheduleToClassChannel(b, db, classID, dayName)
		_ = c.Respond()

		if !success {
			return c.Send(text, tele.ModeHTML)
		}
		return c.Send(text, tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "edit_sch"}, func(c tele.Context) error {
		data := c.Data()
		parts := strings.Split(data, "_")
		classID := parts[0]
		dayName := parts[1]

		userID := c.Sender().ID
		setState(userID, "WAITING_LESSONS_TEXT")
		setTempData(userID, "sch_class_id", classID)
		setTempData(userID, "sch_day", dayName)

		_ = c.Respond()
		return c.Send(fmt.Sprintf("✏️ <b>%s</b> kuni uchun darslar ro'yxatini yuboring:\n\n<b>Misol:</b>\n1. Matematika\n2. Ona tili\n3. Fizika\n4. Ingliz tili", dayName), tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "set_time_type"}, func(c tele.Context) error {
		timeType := c.Data()
		userID := c.Sender().ID
		if timeType == "grade" {
			setState(userID, "WAITING_SET_GRADE_TIME")
			_ = c.Respond()
			return c.Send("📊 <b>Baholarni avtomat yuborish</b> vaqtini kiriting (masalan: <code>14:00</code>):", tele.ModeHTML)
		} else {
			setState(userID, "WAITING_SET_SCHED_TIME")
			_ = c.Respond()
			return c.Send("📅 <b>Ertangi kun dars jadvalini</b> avtomat yuborish vaqtini kiriting (masalan: <code>19:00</code>):", tele.ModeHTML)
		}
	})

	b.Handle(&tele.Btn{Unique: "sel_grd"}, func(c tele.Context) error {
		grade := c.Data()
		userID := c.Sender().ID
		setTempData(userID, "grade", grade)

		inline := &tele.ReplyMarkup{}
		btnA := inline.Data(fmt.Sprintf("%s-A", grade), "sel_ltr", "A")
		btnB := inline.Data(fmt.Sprintf("%s-B", grade), "sel_ltr", "B")
		inline.Inline(inline.Row(btnA, btnB))
		_ = c.Respond()
		return c.Edit(fmt.Sprintf("Sinf: <b>%s</b>. Endi harfni tanlang:", grade), inline, tele.ModeHTML)
	})

	b.Handle(&tele.Btn{Unique: "sel_ltr"}, func(c tele.Context) error {
		letter := c.Data()
		userID := c.Sender().ID
		setTempData(userID, "letter", letter)
		setState(userID, "STUDENT_ENTER_LOGIN")
		_ = c.Respond()
		grade := getTempData(userID, "grade")
		return c.Edit(fmt.Sprintf("Sinf: <b>%s-%s</b> tanlandi.\n\nEndi Kundalik.com <b>Login</b>ini yuboring:", grade, letter), tele.ModeHTML)
	})

	// ==================== BAZANI TIKLASH ====================
	b.Handle(tele.OnDocument, func(c tele.Context) error {
		if c.Sender().ID != cfg.OwnerID {
			return c.Send("⛔ Faqat Owner baza yuklay oladi.")
		}
		doc := c.Message().Document
		if !strings.HasSuffix(strings.ToLower(doc.FileName), ".db") {
			return c.Send("❌ Faqat <b>.db</b> kengaytmali fayl yuboring!", tele.ModeHTML)
		}

		fileReader, err := b.File(&doc.File)
		if err != nil {
			return c.Send("❌ Yuklab olishda xatolik yuz berdi.")
		}
		defer fileReader.Close()

		tempFile := "./bot_restored.db"
		out, _ := os.Create(tempFile)
		_, _ = io.Copy(out, fileReader)
		out.Close()

		_ = db.Conn.Close()
		_ = os.Remove("./bot.db")
		_ = os.Rename(tempFile, "./bot.db")
		db = database.InitDB()

		return c.Send("✅ <b>Baza muvaffaqiyatli tiklandi!</b>", tele.ModeHTML)
	})

	// ==================== FSM MATN XABARLARI ====================
	b.Handle(tele.OnText, func(c tele.Context) error {
		userID := c.Sender().ID
		state := getState(userID)
		text := strings.TrimSpace(c.Text())

		switch state {
		case "WAITING_LESSONS_TEXT":
			classIDStr := getTempData(userID, "sch_class_id")
			dayName := getTempData(userID, "sch_day")
			classID, _ := strconv.Atoi(classIDStr)

			_, err := db.Conn.Exec(`INSERT INTO schedules (class_id, day_of_week, lessons) 
				VALUES (?, ?, ?) 
				ON CONFLICT(class_id, day_of_week) DO UPDATE SET lessons = excluded.lessons`, classID, dayName, text)

			clearState(userID)

			if err != nil {
				return c.Send("❌ Saqlashda xatolik bo'ldi.")
			}
			return c.Send(fmt.Sprintf("✅ <b>%s</b> kuni uchun dars jadvali saqlandi!", dayName), tele.ModeHTML)

		case "WAITING_SET_GRADE_TIME":
			_, err := time.Parse("15:04", text)
			if err != nil {
				return c.Send("❌ Noto'g'ri format! Masalan: <code>14:00</code>", tele.ModeHTML)
			}
			_, _ = db.Conn.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('send_time', ?)", text)
			clearState(userID)
			return c.Send(fmt.Sprintf("✅ <b>Baholar yuborish vaqti:</b> <code>%s</code> ga belgilandi.", text), tele.ModeHTML)

		case "WAITING_SET_SCHED_TIME":
			_, err := time.Parse("15:04", text)
			if err != nil {
				return c.Send("❌ Noto'g'ri format! Masalan: <code>19:00</code>", tele.ModeHTML)
			}
			_, _ = db.Conn.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('schedule_time', ?)", text)
			clearState(userID)
			return c.Send(fmt.Sprintf("✅ <b>Dars jadvali yuborish vaqti:</b> <code>%s</code> ga belgilandi.", text), tele.ModeHTML)

		case "ADMIN_ENTER_ID":
			adminID, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return c.Send("❌ Telegram ID faqat raqam bo'lishi kerak:")
			}
			setTempData(userID, "admin_id", text)
			setState(userID, "ADMIN_ENTER_NAME")
			return c.Send(fmt.Sprintf("ID: <code>%d</code> qabul qilindi.\nIsm va familiyasini kiriting:", adminID), tele.ModeHTML)

		case "ADMIN_ENTER_NAME":
			adminID, _ := strconv.ParseInt(getTempData(userID, "admin_id"), 10, 64)
			_, err := db.Conn.Exec("INSERT INTO admins (telegram_id, full_name) VALUES (?, ?)", adminID, text)
			clearState(userID)
			if err != nil {
				return c.Send("❌ Bu admin bazada mavjud.")
			}
			return c.Send(fmt.Sprintf("✅ Admin qo'shildi: <b>%s</b>", text), tele.ModeHTML)

		case "WAITING_DEL_ADMIN":
			adminID, _ := strconv.ParseInt(text, 10, 64)
			_, _ = db.Conn.Exec("DELETE FROM admins WHERE telegram_id = ?", adminID)
			clearState(userID)
			return c.Send("🗑 Admin o'chirildi.")

		case "CHANNEL_ENTER_ID":
			chID, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return c.Send("❌ Kanal ID faqat raqam bo'lishi kerak:")
			}
			setTempData(userID, "channel_id", strconv.FormatInt(chID, 10))
			setState(userID, "CHANNEL_ENTER_TITLE")
			return c.Send("Kanal nomini yozing (masalan: <code>9-A Sinf Kanali</code>):", tele.ModeHTML)

		case "CHANNEL_ENTER_TITLE":
			chID, _ := strconv.ParseInt(getTempData(userID, "channel_id"), 10, 64)
			_, _ = db.Conn.Exec("INSERT OR REPLACE INTO channels (channel_id, title) VALUES (?, ?)", chID, text)
			clearState(userID)
			return c.Send(fmt.Sprintf("✅ Kanal saqlandi: <b>%s</b>", text), tele.ModeHTML)

		case "STUDENT_ENTER_NAME":
			setTempData(userID, "name", text)
			inline := &tele.ReplyMarkup{}
			var rowsBtn []tele.Row
			var curRow []tele.Btn
			for g := 1; g <= 11; g++ {
				btn := inline.Data(fmt.Sprintf("%d-sinf", g), "sel_grd", strconv.Itoa(g))
				curRow = append(curRow, btn)
				if len(curRow) == 3 || g == 11 {
					rowsBtn = append(rowsBtn, inline.Row(curRow...))
					curRow = []tele.Btn{}
				}
			}
			inline.Inline(rowsBtn...)
			return c.Send(fmt.Sprintf("O'quvchi: <b>%s</b>\nSinf raqamini tanlang:", text), inline, tele.ModeHTML)

		case "STUDENT_ENTER_LOGIN":
			setTempData(userID, "login", text)
			setState(userID, "STUDENT_ENTER_PASS")
			return c.Send("Kundalik.com <b>Parol</b>ini yozing:", tele.ModeHTML)

		case "STUDENT_ENTER_PASS":
			rawPass := text
			name := getTempData(userID, "name")
			grade, _ := strconv.Atoi(getTempData(userID, "grade"))
			letter := getTempData(userID, "letter")
			login := getTempData(userID, "login")

			encryptedPass, err := config.Encrypt(rawPass, cfg.SecretKey)
			if err != nil {
				return c.Send("❌ Parolni shifrlashda xato yuz berdi.")
			}

			var classID int
			var channelID int64
			err = db.Conn.QueryRow("SELECT id, channel_id FROM classes WHERE grade_number = ? AND class_letter = ?", grade, letter).Scan(&classID, &channelID)
			if err != nil {
				res, _ := db.Conn.Exec("INSERT INTO classes (grade_number, class_letter, channel_id) VALUES (?, ?, 0)", grade, letter)
				lastID, _ := res.LastInsertId()
				classID = int(lastID)
			}

			_, err = db.Conn.Exec(`INSERT INTO students (class_id, full_name, kundalik_login, kundalik_password, channel_id, added_by) 
				VALUES (?, ?, ?, ?, ?, ?)`, classID, name, login, encryptedPass, channelID, userID)

			clearState(userID)

			if err != nil {
				return c.Send(fmt.Sprintf("❌ Saqlashda xatolik: %v", err))
			}

			return c.Send(fmt.Sprintf("✅ <b>O'quvchi saqlandi!</b>\n\n👤 Ism: <b>%s</b>\n🏫 Sinf: <b>%d-%s</b>\n🔑 Login: <code>%s</code>",
				name, grade, letter, login), tele.ModeHTML)
		}

		return nil
	})

	log.Println("Bot muvaffaqiyatli ishga tushdi...")
	b.Start()
}