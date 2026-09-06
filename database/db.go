package database

import (
	"database/sql"
	"log"

	_ "modernc.org/sqlite"
)

type DB struct {
	Conn *sql.DB
}

func InitDB() *DB {
	conn, err := sql.Open("sqlite", "./bot.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatal("Bazaga ulanishda xatolik:", err)
	}

	conn.SetMaxOpenConns(10)
	conn.SetMaxIdleConns(5)

	createTablesQuery := `
	PRAGMA journal_mode = WAL;
	PRAGMA busy_timeout = 5000;

	CREATE TABLE IF NOT EXISTS admins (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		telegram_id BIGINT UNIQUE NOT NULL,
		full_name TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS channels (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id BIGINT UNIQUE NOT NULL,
		title TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS classes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		grade_number INTEGER NOT NULL,
		class_letter TEXT NOT NULL,
		channel_id BIGINT DEFAULT 0,
		UNIQUE(grade_number, class_letter)
	);

	CREATE TABLE IF NOT EXISTS students (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		class_id INTEGER NOT NULL,
		full_name TEXT NOT NULL,
		kundalik_login TEXT NOT NULL,
		kundalik_password TEXT NOT NULL,
		channel_id BIGINT NOT NULL,
		added_by BIGINT NOT NULL,
		FOREIGN KEY (class_id) REFERENCES classes(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS schedules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		class_id INTEGER NOT NULL,
		day_of_week TEXT NOT NULL,
		lessons TEXT NOT NULL,
		UNIQUE(class_id, day_of_week),
		FOREIGN KEY (class_id) REFERENCES classes(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`

	_, err = conn.Exec(createTablesQuery)
	if err != nil {
		log.Fatal("Jadvallarni yaratishda xatolik:", err)
	}

	// Boshlang'ich vaqtlar: Baholar uchun 14:00, Jadval uchun 19:00
	_, _ = conn.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES ('send_time', '14:00')`)
	_, _ = conn.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES ('schedule_time', '19:00')`)

	for grade := 1; grade <= 11; grade++ {
		for _, letter := range []string{"A", "B"} {
			_, _ = conn.Exec(`INSERT OR IGNORE INTO classes (grade_number, class_letter, channel_id) VALUES (?, ?, 0)`, grade, letter)
		}
	}

	return &DB{Conn: conn}
}