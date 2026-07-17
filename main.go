package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultEnvPath   = "config/.env"
	defaultDataDir   = "data"
	defaultUploadDir = "data/uploads"
	defaultDBPath    = "../data/app.sqlite"
	defaultPort      = "8097"
	sessionCookie    = "iparent_session"
	sessionLifetime  = 12 * time.Hour
	maxFailures      = 5
	failWindow       = 24 * time.Hour
	rewardImageMax   = 1 << 20
)

type Config struct {
	AdminUsername string
	AdminPassword string
	SessionSecret string
	DBPath        string
	Port          string
	EnvPath       string
	BaseDir       string
}

type App struct {
	cfg      Config
	db       *sql.DB
	sessions *SessionStore
	tpl      *template.Template
}

type Session struct {
	ID        string
	Role      string
	ChildID   int64
	ExpiresAt time.Time
}

type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]Session
}

type Challenge struct {
	ID          int64
	Title       string
	Prompt      string
	Type        string
	Points      int
	Answer      string
	ManualGrade bool
	Options     []Option
	OptionLines string
}

type Option struct {
	ID        int64
	Text      string
	IsCorrect bool
}

type Child struct {
	ID            int64
	Name          string
	Username      string
	Points        int
	Done          int
	Total         int
	Percent       int
	HomeMessage   string
	HomeImagePath string
}

type Reward struct {
	ID        int64
	Title     string
	Points    int
	ImagePath string
}

type RewardPurchase struct {
	ID        int64
	Title     string
	Points    int
	CreatedAt string
}

type StrikeRule struct {
	ID          int64
	Title       string
	Description string
	Points      int
}

type ChallengeSchedule struct {
	ID             int64
	ChildUsername  string
	ChallengeTitle string
	Frequency      string
	NextRun        string
	NextRunUnix    int64
	Weekday        int
	Active         bool
}

type PointEvent struct {
	ChildUsername string
	Kind          string
	Title         string
	Detail        string
	Amount        int
	Balance       int
	CreatedAt     string
	CreatedUnix   int64
}

type AdminPage struct {
	Children      []Child
	Challenges    []Challenge
	EditChallenge Challenge
	EditReward    Reward
	Rewards       []Reward
	Strikes       []StrikeRule
	Schedules     []ChallengeSchedule
	Events        []PointEvent
	Pending       []Submission
	Error         string
}

type ChildPage struct {
	Child               Child
	Challenges          []ChallengeStatus
	CompletedChallenges []ChallengeStatus
	CompletedDetail     ChallengeStatus
	Rewards             []Reward
	Purchases           []RewardPurchase
	Strikes             []StrikeRule
	Events              []PointEvent
	Message             string
	Error               string
}

type ChallengeStatus struct {
	Challenge
	Status        string
	Earned        int
	Submitted     string
	CorrectAnswer string
	SubmittedAt   string
}

type Submission struct {
	ID             int64
	ChildUsername  string
	ChallengeTitle string
	Answer         string
	PhotoPath      string
	PointsPossible int
	CreatedAt      string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		if err := initEnvironment(defaultEnvPath); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Initialized ./config/.env, ./data/app.sqlite, and ./data/uploads. Start with: iparent\n")
		return
	}

	cfg, err := loadConfig(defaultEnvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iparent cannot start: %v\n\nRun `iparent init` from the project directory to create config/.env and data/app.sqlite.\n", err)
		os.Exit(1)
	}
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := migrate(db); err != nil {
		log.Fatal(err)
	}

	app := &App{cfg: cfg, db: db, sessions: NewSessionStore(), tpl: templates()}
	mux := http.NewServeMux()
	app.routes(mux)

	addr := ":" + cfg.Port
	log.Printf("iparent listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func initEnvironment(envPath string) error {
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(defaultDataDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(defaultUploadDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(envPath); err == nil {
		cfg, err := loadConfig(envPath)
		if err != nil {
			return err
		}
		return initDatabase(cfg.DBPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	secret, err := randomToken(32)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("ADMIN_USERNAME=admin\nADMIN_PASSWORD=change-me-now\nSESSION_SECRET=%s\nDB_PATH=%s\nPORT=%s\n", secret, defaultDBPath, defaultPort)
	if err := os.WriteFile(envPath, []byte(body), 0o600); err != nil {
		return err
	}
	cfg, err := loadConfig(envPath)
	if err != nil {
		return err
	}
	return initDatabase(cfg.DBPath)
}

func initDatabase(dbPath string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return migrate(db)
}

func loadConfig(envPath string) (Config, error) {
	raw, err := os.ReadFile(envPath)
	if err != nil {
		return Config{}, fmt.Errorf("missing environment file at %s", envPath)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	cfg := Config{
		AdminUsername: values["ADMIN_USERNAME"],
		AdminPassword: values["ADMIN_PASSWORD"],
		SessionSecret: values["SESSION_SECRET"],
		DBPath:        values["DB_PATH"],
		Port:          values["PORT"],
		EnvPath:       envPath,
		BaseDir:       filepath.Dir(envPath),
	}
	if cfg.AdminUsername == "" || cfg.AdminPassword == "" || cfg.SessionSecret == "" || cfg.DBPath == "" {
		return Config{}, errors.New("ADMIN_USERNAME, ADMIN_PASSWORD, SESSION_SECRET, and DB_PATH are required")
	}
	if !filepath.IsAbs(cfg.DBPath) {
		cfg.DBPath = filepath.Clean(filepath.Join(cfg.BaseDir, cfg.DBPath))
	}
	if cfg.Port == "" {
		cfg.Port = defaultPort
	}
	if port := os.Getenv("PORT"); port != "" {
		cfg.Port = port
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS login_failures (id INTEGER PRIMARY KEY AUTOINCREMENT, ip TEXT NOT NULL, attempted_at INTEGER NOT NULL);`,
		`CREATE INDEX IF NOT EXISTS idx_login_failures_ip_time ON login_failures (ip, attempted_at);`,
		`CREATE TABLE IF NOT EXISTS children (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, created_at INTEGER NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS challenges (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL, prompt TEXT NOT NULL, type TEXT NOT NULL, points INTEGER NOT NULL, answer TEXT NOT NULL DEFAULT '', manual_grade INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS challenge_options (id INTEGER PRIMARY KEY AUTOINCREMENT, challenge_id INTEGER NOT NULL, text TEXT NOT NULL, is_correct INTEGER NOT NULL DEFAULT 0, FOREIGN KEY(challenge_id) REFERENCES challenges(id) ON DELETE CASCADE);`,
		`CREATE TABLE IF NOT EXISTS submissions (id INTEGER PRIMARY KEY AUTOINCREMENT, child_id INTEGER NOT NULL, challenge_id INTEGER NOT NULL, answer TEXT NOT NULL DEFAULT '', photo_path TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, points_awarded INTEGER NOT NULL DEFAULT 0, earns_points INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, reviewed_at INTEGER, FOREIGN KEY(child_id) REFERENCES children(id), FOREIGN KEY(challenge_id) REFERENCES challenges(id));`,
		`CREATE TABLE IF NOT EXISTS challenge_unlocks (id INTEGER PRIMARY KEY AUTOINCREMENT, child_id INTEGER NOT NULL, challenge_id INTEGER, created_at INTEGER NOT NULL, FOREIGN KEY(child_id) REFERENCES children(id));`,
		`CREATE TABLE IF NOT EXISTS rewards (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL, points INTEGER NOT NULL, created_at INTEGER NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS reward_purchases (id INTEGER PRIMARY KEY AUTOINCREMENT, child_id INTEGER NOT NULL, reward_id INTEGER NOT NULL, points_spent INTEGER NOT NULL, created_at INTEGER NOT NULL, FOREIGN KEY(child_id) REFERENCES children(id), FOREIGN KEY(reward_id) REFERENCES rewards(id));`,
		`CREATE TABLE IF NOT EXISTS point_adjustments (id INTEGER PRIMARY KEY AUTOINCREMENT, child_id INTEGER NOT NULL, amount INTEGER NOT NULL, reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, FOREIGN KEY(child_id) REFERENCES children(id));`,
		`CREATE TABLE IF NOT EXISTS strike_rules (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', points INTEGER NOT NULL, created_at INTEGER NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS strike_events (id INTEGER PRIMARY KEY AUTOINCREMENT, child_id INTEGER NOT NULL, strike_rule_id INTEGER, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', points_deducted INTEGER NOT NULL, created_at INTEGER NOT NULL, FOREIGN KEY(child_id) REFERENCES children(id), FOREIGN KEY(strike_rule_id) REFERENCES strike_rules(id));`,
		`CREATE TABLE IF NOT EXISTS child_home_settings (child_id INTEGER PRIMARY KEY, message TEXT NOT NULL DEFAULT '', image_path TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL DEFAULT 0, FOREIGN KEY(child_id) REFERENCES children(id));`,
		`CREATE TABLE IF NOT EXISTS challenge_schedules (id INTEGER PRIMARY KEY AUTOINCREMENT, child_id INTEGER NOT NULL, challenge_id INTEGER NOT NULL, frequency TEXT NOT NULL, next_run INTEGER NOT NULL, weekday INTEGER NOT NULL DEFAULT -1, active INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, last_run INTEGER NOT NULL DEFAULT 0, FOREIGN KEY(child_id) REFERENCES children(id), FOREIGN KEY(challenge_id) REFERENCES challenges(id));`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	if err := ensureColumn(db, "rewards", "image_path", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "children", "name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]Session{}}
}

func (s *SessionStore) Create(role string, childID int64) (Session, error) {
	id, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	session := Session{ID: id, Role: role, ChildID: childID, ExpiresAt: time.Now().Add(sessionLifetime)}
	s.mu.Lock()
	s.sessions[id] = session
	s.mu.Unlock()
	return session, nil
}

func (s *SessionStore) Get(id string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok || time.Now().After(session.ExpiresAt) {
		delete(s.sessions, id)
		return Session{}, false
	}
	return session, true
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (a *App) routes(mux *http.ServeMux) {
	mux.HandleFunc("/", a.home)
	mux.HandleFunc("/login", a.login)
	mux.HandleFunc("/logout", a.logout)
	mux.HandleFunc("/admin", a.requireAdmin(a.admin))
	mux.HandleFunc("/admin/children", a.requireAdmin(a.adminChildren))
	mux.HandleFunc("/admin/children/name", a.requireAdmin(a.updateChildName))
	mux.HandleFunc("/admin/challenges", a.requireAdmin(a.adminChallenges))
	mux.HandleFunc("/admin/challenges/", a.requireAdmin(a.adminChallengeEdit))
	mux.HandleFunc("/admin/schedules", a.requireAdmin(a.adminSchedules))
	mux.HandleFunc("/admin/rewards", a.requireAdmin(a.adminRewards))
	mux.HandleFunc("/admin/rewards/redeem", a.requireAdmin(a.redeemReward))
	mux.HandleFunc("/admin/rewards/", a.requireAdmin(a.adminRewardEdit))
	mux.HandleFunc("/admin/review", a.requireAdmin(a.adminReview))
	mux.HandleFunc("/admin/points", a.requireAdmin(a.adminPoints))
	mux.HandleFunc("/admin/points/adjust", a.requireAdmin(a.adjustChildPoints))
	mux.HandleFunc("/admin/points/gift", a.requireAdmin(a.giftChildPoints))
	mux.HandleFunc("/admin/history", a.requireAdmin(a.adminHistory))
	mux.HandleFunc("/admin/strikes", a.requireAdmin(a.adminStrikes))
	mux.HandleFunc("/admin/strikes/impose", a.requireAdmin(a.imposeStrike))
	mux.HandleFunc("/admin/children/home", a.requireAdmin(a.updateChildHome))
	mux.HandleFunc("/admin/reset", a.requireAdmin(a.resetChild))
	mux.HandleFunc("/child", a.requireChild(a.childDashboard))
	mux.HandleFunc("/child/completed", a.requireChild(a.childCompleted))
	mux.HandleFunc("/child/completed/", a.requireChild(a.childCompletedDetail))
	mux.HandleFunc("/child/rewards", a.requireChild(a.childRewards))
	mux.HandleFunc("/child/rewards/buy", a.requireChild(a.childRewardRedeemUnavailable))
	mux.HandleFunc("/child/points", a.requireChild(a.childPointsHistory))
	mux.HandleFunc("/child/history", a.requireChild(a.childHistory))
	mux.HandleFunc("/child/submit", a.requireChild(a.submitChallenge))
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("assets"))))
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir("data/uploads"))))
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	if session, ok := a.currentSession(r); ok {
		if session.Role == "admin" {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/child", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.render(w, "login", map[string]string{})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	blocked, err := a.isBlocked(ip)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if blocked {
		http.Error(w, "too many login attempts", http.StatusForbidden)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if username == a.cfg.AdminUsername && password == a.cfg.AdminPassword {
		a.startSession(w, r, "admin", 0, "/admin")
		return
	}
	childID, ok := a.validChild(username, password)
	if ok {
		a.startSession(w, r, "child", childID, "/child")
		return
	}
	blocked, err = a.recordFailure(ip)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	if blocked {
		http.Error(w, "too many login attempts", http.StatusForbidden)
		return
	}
	a.render(w, "login", map[string]string{"Error": "Invalid username or password."})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if id, ok := a.verifyCookie(c.Value); ok {
			a.sessions.Delete(id)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) admin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/children", http.StatusSeeOther)
}

func (a *App) adminChildren(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		page, err := a.adminPage("")
		if err != nil {
			http.Error(w, "admin unavailable", http.StatusInternalServerError)
			return
		}
		a.render(w, "admin_children", page)
		return
	}
	a.createChild(w, r)
}

func (a *App) adminChallenges(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		page, err := a.adminPage("")
		if err != nil {
			http.Error(w, "admin unavailable", http.StatusInternalServerError)
			return
		}
		a.render(w, "admin_challenges", page)
		return
	}
	a.createChallenge(w, r)
}

func (a *App) adminSchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		page, err := a.adminPage("")
		if err != nil {
			http.Error(w, "schedules unavailable", http.StatusInternalServerError)
			return
		}
		a.render(w, "admin_schedules", page)
		return
	}
	a.createChallengeSchedule(w, r)
}

func (a *App) createChallengeSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	childID, _ := strconv.ParseInt(r.FormValue("child_id"), 10, 64)
	challengeID, _ := strconv.ParseInt(r.FormValue("challenge_id"), 10, 64)
	frequency := r.FormValue("frequency")
	weekday, _ := strconv.Atoi(r.FormValue("weekday"))
	nextRun, normalizedWeekday, err := nextScheduleRun(frequency, r.FormValue("date"), weekday, time.Now())
	if childID < 1 || challengeID < 1 || err != nil {
		a.renderAdminError(w, "admin_schedules", "Choose a child, challenge, and valid schedule.")
		return
	}
	_, _ = a.db.Exec(`INSERT INTO challenge_schedules (child_id, challenge_id, frequency, next_run, weekday, active, created_at) VALUES (?, ?, ?, ?, ?, 1, ?)`, childID, challengeID, frequency, nextRun, normalizedWeekday, time.Now().Unix())
	http.Redirect(w, r, "/admin/schedules", http.StatusSeeOther)
}

func (a *App) adminChallengeEdit(w http.ResponseWriter, r *http.Request) {
	idText := strings.TrimPrefix(r.URL.Path, "/admin/challenges/")
	challengeID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || challengeID < 1 {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		a.renderChallengeEdit(w, challengeID, "")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	challengeType := r.FormValue("type")
	points, _ := strconv.Atoi(r.FormValue("points"))
	answer := strings.TrimSpace(r.FormValue("answer"))
	manual := r.FormValue("manual_grade") == "on" || challengeType == "photo" || challengeType == "long_answer"
	if title == "" || prompt == "" || points < 1 {
		a.renderChallengeEdit(w, challengeID, "Challenge title, prompt, and positive points are required.")
		return
	}
	res, err := a.db.Exec(`UPDATE challenges SET title=?, prompt=?, type=?, points=?, answer=?, manual_grade=? WHERE id=?`, title, prompt, challengeType, points, answer, boolInt(manual), challengeID)
	if err != nil {
		a.renderChallengeEdit(w, challengeID, "Could not update challenge.")
		return
	}
	changed, _ := res.RowsAffected()
	if changed == 0 {
		http.NotFound(w, r)
		return
	}
	if err := a.replaceChallengeOptions(challengeID, r.FormValue("options")); err != nil {
		a.renderChallengeEdit(w, challengeID, "Could not update challenge options.")
		return
	}
	http.Redirect(w, r, "/admin/challenges", http.StatusSeeOther)
}

func (a *App) renderChallengeEdit(w http.ResponseWriter, challengeID int64, msg string) {
	page, err := a.adminPage(msg)
	if err != nil {
		http.Error(w, "admin unavailable", http.StatusInternalServerError)
		return
	}
	challenge, err := a.challenge(challengeID)
	if err != nil {
		http.Error(w, "challenge not found", http.StatusNotFound)
		return
	}
	page.EditChallenge = challenge
	a.render(w, "admin_challenge_edit", page)
}

func (a *App) adminRewards(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		page, err := a.adminPage("")
		if err != nil {
			http.Error(w, "admin unavailable", http.StatusInternalServerError)
			return
		}
		a.render(w, "admin_rewards", page)
		return
	}
	a.createReward(w, r)
}

func (a *App) adminRewardEdit(w http.ResponseWriter, r *http.Request) {
	idText := strings.TrimPrefix(r.URL.Path, "/admin/rewards/")
	rewardID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || rewardID < 1 {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		a.renderRewardEdit(w, rewardID, "")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, rewardImageMax+64*1024)
	if err := r.ParseMultipartForm(rewardImageMax); err != nil {
		a.renderRewardEdit(w, rewardID, "Reward images must be 1 MB or smaller.")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	points, _ := strconv.Atoi(r.FormValue("points"))
	if title == "" || points < 1 {
		a.renderRewardEdit(w, rewardID, "Reward title and positive point cost are required.")
		return
	}
	imagePath, err := saveUploadedRewardImage(r, "image")
	if err != nil {
		a.renderRewardEdit(w, rewardID, err.Error())
		return
	}
	if imagePath == "" {
		res, err := a.db.Exec(`UPDATE rewards SET title=?, points=? WHERE id=?`, title, points, rewardID)
		if err != nil {
			a.renderRewardEdit(w, rewardID, "Could not update reward.")
			return
		}
		changed, _ := res.RowsAffected()
		if changed == 0 {
			http.NotFound(w, r)
			return
		}
	} else {
		res, err := a.db.Exec(`UPDATE rewards SET title=?, points=?, image_path=? WHERE id=?`, title, points, imagePath, rewardID)
		if err != nil {
			a.renderRewardEdit(w, rewardID, "Could not update reward.")
			return
		}
		changed, _ := res.RowsAffected()
		if changed == 0 {
			http.NotFound(w, r)
			return
		}
	}
	http.Redirect(w, r, "/admin/rewards", http.StatusSeeOther)
}

func (a *App) renderRewardEdit(w http.ResponseWriter, rewardID int64, msg string) {
	page, err := a.adminPage(msg)
	if err != nil {
		http.Error(w, "admin unavailable", http.StatusInternalServerError)
		return
	}
	reward, err := a.reward(rewardID)
	if err != nil {
		http.Error(w, "reward not found", http.StatusNotFound)
		return
	}
	page.EditReward = reward
	a.render(w, "admin_reward_edit", page)
}

func (a *App) redeemReward(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	childID, _ := strconv.ParseInt(r.FormValue("child_id"), 10, 64)
	rewardID, _ := strconv.ParseInt(r.FormValue("reward_id"), 10, 64)
	var cost int
	err := a.db.QueryRow(`SELECT points FROM rewards WHERE id=?`, rewardID).Scan(&cost)
	if childID < 1 || err != nil {
		a.renderAdminError(w, "admin_rewards", "Choose a child and a reward to redeem.")
		return
	}
	var alreadyRedeemed int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM reward_purchases WHERE child_id=? AND reward_id=?`, childID, rewardID).Scan(&alreadyRedeemed)
	if alreadyRedeemed > 0 {
		a.renderAdminError(w, "admin_rewards", "That reward has already been redeemed for this child.")
		return
	}
	if a.childPoints(childID) < cost {
		a.renderAdminError(w, "admin_rewards", "That child does not have enough points for this reward.")
		return
	}
	_, _ = a.db.Exec(`INSERT INTO reward_purchases (child_id, reward_id, points_spent, created_at) VALUES (?, ?, ?, ?)`, childID, rewardID, cost, time.Now().Unix())
	http.Redirect(w, r, "/admin/rewards", http.StatusSeeOther)
}

func (a *App) adminReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.reviewSubmission(w, r)
		return
	}
	page, err := a.adminPage("")
	if err != nil {
		http.Error(w, "admin unavailable", http.StatusInternalServerError)
		return
	}
	a.render(w, "admin_review", page)
}

func (a *App) adminPoints(w http.ResponseWriter, r *http.Request) {
	page, err := a.adminPage("")
	if err != nil {
		http.Error(w, "points unavailable", http.StatusInternalServerError)
		return
	}
	a.render(w, "admin_points", page)
}

func (a *App) adminHistory(w http.ResponseWriter, r *http.Request) {
	page, err := a.adminPage("")
	if err != nil {
		http.Error(w, "history unavailable", http.StatusInternalServerError)
		return
	}
	a.render(w, "admin_history", page)
}

func (a *App) adjustChildPoints(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	childID, _ := strconv.ParseInt(r.FormValue("child_id"), 10, 64)
	amount, _ := strconv.Atoi(r.FormValue("amount"))
	reason := strings.TrimSpace(r.FormValue("reason"))
	if childID < 1 || amount == 0 {
		a.renderAdminError(w, "admin_points", "Choose a child and enter a non-zero point adjustment.")
		return
	}
	if reason == "" {
		reason = "Parent point adjustment"
	}
	_, _ = a.db.Exec(`INSERT INTO point_adjustments (child_id, amount, reason, created_at) VALUES (?, ?, ?, ?)`, childID, amount, reason, time.Now().Unix())
	http.Redirect(w, r, "/admin/points", http.StatusSeeOther)
}

func (a *App) giftChildPoints(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	childID, _ := strconv.ParseInt(r.FormValue("child_id"), 10, 64)
	amount, _ := strconv.Atoi(r.FormValue("amount"))
	reason := strings.TrimSpace(r.FormValue("reason"))
	if childID < 1 || amount < 1 {
		a.renderAdminError(w, "admin_points", "Choose a child and enter a positive gift amount.")
		return
	}
	if reason == "" {
		reason = "Gift from parent"
	} else {
		reason = "Gift: " + reason
	}
	_, _ = a.db.Exec(`INSERT INTO point_adjustments (child_id, amount, reason, created_at) VALUES (?, ?, ?, ?)`, childID, amount, reason, time.Now().Unix())
	http.Redirect(w, r, "/admin/history", http.StatusSeeOther)
}

func (a *App) updateChildHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, rewardImageMax+64*1024)
	if err := r.ParseMultipartForm(rewardImageMax); err != nil {
		a.renderAdminError(w, "admin_children", "Homepage pictures must be 1 MB or smaller.")
		return
	}
	childID, _ := strconv.ParseInt(r.FormValue("child_id"), 10, 64)
	message := strings.TrimSpace(r.FormValue("message"))
	if childID < 1 {
		a.renderAdminError(w, "admin_children", "Choose a child before saving a homepage note.")
		return
	}
	imagePath, err := saveUploadedImage(r, "image", "data/uploads/child-home", "/uploads/child-home")
	if err != nil {
		a.renderAdminError(w, "admin_children", err.Error())
		return
	}
	if imagePath == "" {
		_, _ = a.db.Exec(`INSERT INTO child_home_settings (child_id, message, updated_at) VALUES (?, ?, ?) ON CONFLICT(child_id) DO UPDATE SET message=excluded.message, updated_at=excluded.updated_at`, childID, message, time.Now().Unix())
	} else {
		_, _ = a.db.Exec(`INSERT INTO child_home_settings (child_id, message, image_path, updated_at) VALUES (?, ?, ?, ?) ON CONFLICT(child_id) DO UPDATE SET message=excluded.message, image_path=excluded.image_path, updated_at=excluded.updated_at`, childID, message, imagePath, time.Now().Unix())
	}
	http.Redirect(w, r, "/admin/children", http.StatusSeeOther)
}

func (a *App) adminStrikes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.createStrikeRule(w, r)
		return
	}
	page, err := a.adminPage("")
	if err != nil {
		http.Error(w, "strikes unavailable", http.StatusInternalServerError)
		return
	}
	a.render(w, "admin_strikes", page)
}

func (a *App) createStrikeRule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	description := strings.TrimSpace(r.FormValue("description"))
	points, _ := strconv.Atoi(r.FormValue("points"))
	if title == "" || points < 1 {
		a.renderAdminError(w, "admin_strikes", "Strike name and positive point deduction are required.")
		return
	}
	_, _ = a.db.Exec(`INSERT INTO strike_rules (title, description, points, created_at) VALUES (?, ?, ?, ?)`, title, description, points, time.Now().Unix())
	http.Redirect(w, r, "/admin/strikes", http.StatusSeeOther)
}

func (a *App) imposeStrike(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	childID, _ := strconv.ParseInt(r.FormValue("child_id"), 10, 64)
	strikeID, _ := strconv.ParseInt(r.FormValue("strike_id"), 10, 64)
	var s StrikeRule
	err := a.db.QueryRow(`SELECT id, title, description, points FROM strike_rules WHERE id=?`, strikeID).Scan(&s.ID, &s.Title, &s.Description, &s.Points)
	if childID < 1 || err != nil {
		a.renderAdminError(w, "admin_strikes", "Choose a child and a strike to apply.")
		return
	}
	_, _ = a.db.Exec(`INSERT INTO strike_events (child_id, strike_rule_id, title, description, points_deducted, created_at) VALUES (?, ?, ?, ?, ?, ?)`, childID, s.ID, s.Title, s.Description, s.Points, time.Now().Unix())
	http.Redirect(w, r, "/admin/strikes", http.StatusSeeOther)
}

func (a *App) createChild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if name == "" || username == "" || password == "" {
		a.renderAdminError(w, "admin_children", "Child name, username, and password are required.")
		return
	}
	_, err := a.db.Exec(`INSERT INTO children (name, username, password_hash, created_at) VALUES (?, ?, ?, ?)`, name, username, a.hashPassword(password), time.Now().Unix())
	if err != nil {
		a.renderAdminError(w, "admin_children", "That child username is already in use.")
		return
	}
	http.Redirect(w, r, "/admin/children", http.StatusSeeOther)
}

func (a *App) updateChildName(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	childID, _ := strconv.ParseInt(r.FormValue("child_id"), 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	if childID < 1 || name == "" {
		a.renderAdminError(w, "admin_children", "Choose a child and enter their name.")
		return
	}
	result, err := a.db.Exec(`UPDATE children SET name=? WHERE id=?`, name, childID)
	if err != nil {
		a.renderAdminError(w, "admin_children", "The child's name could not be saved.")
		return
	}
	updated, _ := result.RowsAffected()
	if updated != 1 {
		a.renderAdminError(w, "admin_children", "Child not found.")
		return
	}
	http.Redirect(w, r, "/admin/children", http.StatusSeeOther)
}

func (a *App) createReward(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, rewardImageMax+64*1024)
	if err := r.ParseMultipartForm(rewardImageMax); err != nil {
		a.renderAdminError(w, "admin_rewards", "Reward images must be 1 MB or smaller.")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	points, _ := strconv.Atoi(r.FormValue("points"))
	if title == "" || points < 1 {
		a.renderAdminError(w, "admin_rewards", "Reward title and positive point cost are required.")
		return
	}
	imagePath, err := saveUploadedRewardImage(r, "image")
	if err != nil {
		a.renderAdminError(w, "admin_rewards", err.Error())
		return
	}
	_, _ = a.db.Exec(`INSERT INTO rewards (title, points, image_path, created_at) VALUES (?, ?, ?, ?)`, title, points, imagePath, time.Now().Unix())
	http.Redirect(w, r, "/admin/rewards", http.StatusSeeOther)
}

func (a *App) createChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	challengeType := r.FormValue("type")
	points, _ := strconv.Atoi(r.FormValue("points"))
	answer := strings.TrimSpace(r.FormValue("answer"))
	manual := r.FormValue("manual_grade") == "on" || challengeType == "photo" || challengeType == "long_answer"
	if title == "" || prompt == "" || points < 1 {
		a.renderAdminError(w, "admin_challenges", "Challenge title, prompt, and positive points are required.")
		return
	}
	res, err := a.db.Exec(`INSERT INTO challenges (title, prompt, type, points, answer, manual_grade, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, title, prompt, challengeType, points, answer, boolInt(manual), time.Now().Unix())
	if err != nil {
		a.renderAdminError(w, "admin_challenges", "Could not create challenge.")
		return
	}
	challengeID, _ := res.LastInsertId()
	if err := a.replaceChallengeOptions(challengeID, r.FormValue("options")); err != nil {
		a.renderAdminError(w, "admin_challenges", "Could not create challenge options.")
		return
	}
	http.Redirect(w, r, "/admin/challenges", http.StatusSeeOther)
}

func (a *App) reviewSubmission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("submission_id"), 10, 64)
	credit, _ := strconv.ParseFloat(r.FormValue("credit"), 64)
	if credit < 0 {
		credit = 0
	}
	if credit > 100 {
		credit = 100
	}
	var possible int
	err := a.db.QueryRow(`SELECT c.points FROM submissions s JOIN challenges c ON c.id=s.challenge_id WHERE s.id=?`, id).Scan(&possible)
	if err != nil {
		http.Redirect(w, r, "/admin/review", http.StatusSeeOther)
		return
	}
	points := int(math.Round(float64(possible) * credit / 100))
	status := "approved"
	if points == 0 {
		status = "redo"
	}
	_, _ = a.db.Exec(`UPDATE submissions SET status=?, points_awarded=?, earns_points=1, reviewed_at=? WHERE id=?`, status, points, time.Now().Unix(), id)
	http.Redirect(w, r, "/admin/review", http.StatusSeeOther)
}

func (a *App) resetChild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	childID, _ := strconv.ParseInt(r.FormValue("child_id"), 10, 64)
	challengeID, _ := strconv.ParseInt(r.FormValue("challenge_id"), 10, 64)
	var nullable any
	if challengeID > 0 {
		nullable = challengeID
	}
	_, _ = a.db.Exec(`INSERT INTO challenge_unlocks (child_id, challenge_id, created_at) VALUES (?, ?, ?)`, childID, nullable, time.Now().Unix())
	http.Redirect(w, r, "/admin/children", http.StatusSeeOther)
}

func (a *App) childDashboard(w http.ResponseWriter, r *http.Request) {
	session, _ := a.currentSession(r)
	page, err := a.childPage(session.ChildID, r.URL.Query().Get("message"))
	if err != nil {
		http.Error(w, "child dashboard unavailable", http.StatusInternalServerError)
		return
	}
	a.render(w, "child_challenges", page)
}

func (a *App) childCompleted(w http.ResponseWriter, r *http.Request) {
	session, _ := a.currentSession(r)
	page, err := a.childPage(session.ChildID, r.URL.Query().Get("message"))
	if err != nil {
		http.Error(w, "completed challenges unavailable", http.StatusInternalServerError)
		return
	}
	a.render(w, "child_completed", page)
}

func (a *App) childCompletedDetail(w http.ResponseWriter, r *http.Request) {
	session, _ := a.currentSession(r)
	idText := strings.TrimPrefix(r.URL.Path, "/child/completed/")
	challengeID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || challengeID < 1 {
		http.NotFound(w, r)
		return
	}
	page, err := a.childPage(session.ChildID, "")
	if err != nil {
		http.Error(w, "completed challenge unavailable", http.StatusInternalServerError)
		return
	}
	detail, ok := a.completedChallengeDetail(session.ChildID, challengeID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	page.CompletedDetail = detail
	a.render(w, "child_completed_detail", page)
}

func (a *App) childRewards(w http.ResponseWriter, r *http.Request) {
	session, _ := a.currentSession(r)
	page, err := a.childPage(session.ChildID, r.URL.Query().Get("message"))
	if err != nil {
		http.Error(w, "rewards unavailable", http.StatusInternalServerError)
		return
	}
	a.render(w, "child_rewards", page)
}

func (a *App) childRewardRedeemUnavailable(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/child/rewards?message="+template.URLQueryEscaper("Ask your parent to redeem rewards for you"), http.StatusSeeOther)
}

func (a *App) childPointsHistory(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/child/history", http.StatusSeeOther)
}

func (a *App) childHistory(w http.ResponseWriter, r *http.Request) {
	session, _ := a.currentSession(r)
	page, err := a.childPage(session.ChildID, r.URL.Query().Get("message"))
	if err != nil {
		http.Error(w, "history unavailable", http.StatusInternalServerError)
		return
	}
	a.render(w, "child_history", page)
}

func (a *App) submitChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session, _ := a.currentSession(r)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "invalid submission", http.StatusBadRequest)
		return
	}
	challengeID, _ := strconv.ParseInt(r.FormValue("challenge_id"), 10, 64)
	challenge, err := a.challenge(challengeID)
	if err != nil {
		http.Redirect(w, r, "/child?message=Challenge+not+found", http.StatusSeeOther)
		return
	}
	if !a.challengeIsActive(session.ChildID, challengeID) {
		http.Redirect(w, r, "/child?message=Ask+for+this+challenge+to+be+unlocked", http.StatusSeeOther)
		return
	}
	answer := strings.TrimSpace(r.FormValue("answer"))
	if challenge.Type == "select_all" {
		answer = strings.Join(r.Form["answer"], ",")
	}
	photoPath := ""
	if file, header, err := r.FormFile("photo"); err == nil {
		defer file.Close()
		photoPath = filepath.Join("data/uploads", fmt.Sprintf("%d-%d-%s", session.ChildID, time.Now().UnixNano(), filepath.Base(header.Filename)))
		dst, err := os.Create(photoPath)
		if err == nil {
			_, _ = io.Copy(dst, file)
			_ = dst.Close()
		}
	}
	canEarn := a.canEarnPoints(session.ChildID, challengeID)
	status := "approved"
	points := 0
	earns := 0
	if challenge.ManualGrade {
		status = "pending"
	} else if a.answerMatches(challenge, answer) {
		if canEarn {
			points = challenge.Points
			earns = 1
		}
	} else {
		status = "redo"
	}
	_, _ = a.db.Exec(`INSERT INTO submissions (child_id, challenge_id, answer, photo_path, status, points_awarded, earns_points, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, session.ChildID, challengeID, answer, photoPath, status, points, earns, time.Now().Unix())
	msg := "Submitted"
	if status == "redo" {
		msg = "Try again"
	}
	if status == "pending" {
		msg = "Submitted for review"
	}
	http.Redirect(w, r, "/child?message="+template.URLQueryEscaper(msg), http.StatusSeeOther)
}

func (a *App) adminPage(errMsg string) (AdminPage, error) {
	_ = a.runDueSchedules()
	children, err := a.children()
	if err != nil {
		return AdminPage{}, err
	}
	challenges, err := a.challenges()
	if err != nil {
		return AdminPage{}, err
	}
	rewards, err := a.rewards()
	if err != nil {
		return AdminPage{}, err
	}
	pending, err := a.pendingSubmissions()
	if err != nil {
		return AdminPage{}, err
	}
	strikes, err := a.strikeRules()
	if err != nil {
		return AdminPage{}, err
	}
	events, err := a.pointEvents(0, 80)
	if err != nil {
		return AdminPage{}, err
	}
	schedules, err := a.challengeSchedules()
	if err != nil {
		return AdminPage{}, err
	}
	return AdminPage{Children: children, Challenges: challenges, Rewards: rewards, Strikes: strikes, Schedules: schedules, Events: events, Pending: pending, Error: errMsg}, nil
}

func (a *App) renderAdminError(w http.ResponseWriter, templateName, msg string) {
	page, err := a.adminPage(msg)
	if err != nil {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	a.render(w, templateName, page)
}

func (a *App) childPage(childID int64, message string) (ChildPage, error) {
	_ = a.runDueSchedules()
	var child Child
	err := a.db.QueryRow(`SELECT id, name, username FROM children WHERE id=?`, childID).Scan(&child.ID, &child.Name, &child.Username)
	if err != nil {
		return ChildPage{}, err
	}
	child.Points = a.childPoints(childID)
	child.HomeMessage, child.HomeImagePath = a.childHomeSettings(childID)
	challenges, err := a.challenges()
	if err != nil {
		return ChildPage{}, err
	}
	active := []ChallengeStatus{}
	completed := []ChallengeStatus{}
	done := 0
	for _, c := range challenges {
		status, earned := a.challengeStatus(childID, c.ID)
		if status == "approved" || status == "pending" {
			done++
		}
		if a.challengeIsActive(childID, c.ID) {
			active = append(active, ChallengeStatus{Challenge: c, Status: status, Earned: earned})
		}
		if detail, ok := a.completedChallengeDetail(childID, c.ID); ok {
			completed = append(completed, detail)
		}
	}
	child.Done = done
	child.Total = done + len(active)
	if child.Total > 0 {
		child.Percent = int(math.Round(float64(done) / float64(child.Total) * 100))
	}
	rewards, err := a.availableRewardsForChild(childID)
	if err != nil {
		return ChildPage{}, err
	}
	purchases, err := a.rewardPurchases(childID)
	if err != nil {
		return ChildPage{}, err
	}
	strikes, err := a.strikeRules()
	if err != nil {
		return ChildPage{}, err
	}
	events, err := a.pointEvents(childID, 80)
	if err != nil {
		return ChildPage{}, err
	}
	return ChildPage{Child: child, Challenges: active, CompletedChallenges: completed, Rewards: rewards, Purchases: purchases, Strikes: strikes, Events: events, Message: message}, nil
}

func (a *App) children() ([]Child, error) {
	rows, err := a.db.Query(`SELECT id, name, username FROM children ORDER BY CASE WHEN name='' THEN username ELSE name END`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Child
	for rows.Next() {
		var c Child
		if err := rows.Scan(&c.ID, &c.Name, &c.Username); err != nil {
			return nil, err
		}
		c.Points = a.childPoints(c.ID)
		c.HomeMessage, c.HomeImagePath = a.childHomeSettings(c.ID)
		c.Done, c.Total, c.Percent = a.progress(c.ID)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (a *App) childHomeSettings(childID int64) (string, string) {
	var message string
	var imagePath string
	_ = a.db.QueryRow(`SELECT message, image_path FROM child_home_settings WHERE child_id=?`, childID).Scan(&message, &imagePath)
	return message, imagePath
}

func (a *App) challenges() ([]Challenge, error) {
	rows, err := a.db.Query(`SELECT id, title, prompt, type, points, answer, manual_grade FROM challenges ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Challenge
	for rows.Next() {
		var c Challenge
		var manual int
		if err := rows.Scan(&c.ID, &c.Title, &c.Prompt, &c.Type, &c.Points, &c.Answer, &manual); err != nil {
			return nil, err
		}
		c.ManualGrade = manual == 1
		c.Options, _ = a.options(c.ID)
		c.OptionLines = optionLines(c.Options)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (a *App) challenge(id int64) (Challenge, error) {
	var c Challenge
	var manual int
	err := a.db.QueryRow(`SELECT id, title, prompt, type, points, answer, manual_grade FROM challenges WHERE id=?`, id).Scan(&c.ID, &c.Title, &c.Prompt, &c.Type, &c.Points, &c.Answer, &manual)
	if err != nil {
		return Challenge{}, err
	}
	c.ManualGrade = manual == 1
	c.Options, _ = a.options(c.ID)
	c.OptionLines = optionLines(c.Options)
	return c, nil
}

func (a *App) replaceChallengeOptions(challengeID int64, raw string) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM challenge_options WHERE challenge_id=?`, challengeID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		correct := strings.HasPrefix(line, "*")
		line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
		if _, err := tx.Exec(`INSERT INTO challenge_options (challenge_id, text, is_correct) VALUES (?, ?, ?)`, challengeID, line, boolInt(correct)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func optionLines(options []Option) string {
	lines := []string{}
	for _, o := range options {
		line := o.Text
		if o.IsCorrect {
			line = "*" + line
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (a *App) options(challengeID int64) ([]Option, error) {
	rows, err := a.db.Query(`SELECT id, text, is_correct FROM challenge_options WHERE challenge_id=? ORDER BY id`, challengeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Option
	for rows.Next() {
		var o Option
		var correct int
		if err := rows.Scan(&o.ID, &o.Text, &correct); err != nil {
			return nil, err
		}
		o.IsCorrect = correct == 1
		out = append(out, o)
	}
	return out, rows.Err()
}

func (a *App) rewards() ([]Reward, error) {
	rows, err := a.db.Query(`SELECT id, title, points, image_path FROM rewards ORDER BY points`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reward
	for rows.Next() {
		var r Reward
		if err := rows.Scan(&r.ID, &r.Title, &r.Points, &r.ImagePath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (a *App) availableRewardsForChild(childID int64) ([]Reward, error) {
	rows, err := a.db.Query(`SELECT r.id, r.title, r.points, r.image_path FROM rewards r WHERE NOT EXISTS (SELECT 1 FROM reward_purchases rp WHERE rp.child_id=? AND rp.reward_id=r.id) ORDER BY r.points`, childID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reward
	for rows.Next() {
		var r Reward
		if err := rows.Scan(&r.ID, &r.Title, &r.Points, &r.ImagePath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (a *App) reward(id int64) (Reward, error) {
	var r Reward
	err := a.db.QueryRow(`SELECT id, title, points, image_path FROM rewards WHERE id=?`, id).Scan(&r.ID, &r.Title, &r.Points, &r.ImagePath)
	if err != nil {
		return Reward{}, err
	}
	return r, nil
}

func saveUploadedRewardImage(r *http.Request, field string) (string, error) {
	return saveUploadedImage(r, field, "data/uploads/rewards", "/uploads/rewards")
}

func saveUploadedImage(r *http.Request, field, dir, urlPrefix string) (string, error) {
	file, header, err := r.FormFile(field)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) {
			return "", nil
		}
		return "", errors.New("Could not read reward image.")
	}
	defer file.Close()
	if header.Size > rewardImageMax {
		return "", errors.New("Reward images must be 1 MB or smaller.")
	}
	contentType := header.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		return "", errors.New("Uploaded picture must be an image file.")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errors.New("Could not save uploaded picture.")
	}
	name := fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(header.Filename))
	dstPath := filepath.Join(dir, name)
	dst, err := os.Create(dstPath)
	if err != nil {
		return "", errors.New("Could not save uploaded picture.")
	}
	defer dst.Close()
	limited := io.LimitReader(file, rewardImageMax+1)
	n, err := io.Copy(dst, limited)
	if err != nil {
		return "", errors.New("Could not save uploaded picture.")
	}
	if n > rewardImageMax {
		_ = os.Remove(dstPath)
		return "", errors.New("Reward images must be 1 MB or smaller.")
	}
	return strings.TrimRight(urlPrefix, "/") + "/" + name, nil
}

func (a *App) rewardPurchases(childID int64) ([]RewardPurchase, error) {
	rows, err := a.db.Query(`SELECT rp.id, r.title, rp.points_spent, rp.created_at FROM reward_purchases rp JOIN rewards r ON r.id=rp.reward_id WHERE rp.child_id=? ORDER BY rp.created_at DESC`, childID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RewardPurchase
	for rows.Next() {
		var p RewardPurchase
		var ts int64
		if err := rows.Scan(&p.ID, &p.Title, &p.Points, &ts); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(ts, 0).Format("Jan 2 3:04 PM")
		out = append(out, p)
	}
	return out, rows.Err()
}

func (a *App) pendingSubmissions() ([]Submission, error) {
	rows, err := a.db.Query(`SELECT s.id, ch.username, c.title, s.answer, s.photo_path, c.points, s.created_at FROM submissions s JOIN children ch ON ch.id=s.child_id JOIN challenges c ON c.id=s.challenge_id WHERE s.status='pending' ORDER BY s.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		var s Submission
		var ts int64
		if err := rows.Scan(&s.ID, &s.ChildUsername, &s.ChallengeTitle, &s.Answer, &s.PhotoPath, &s.PointsPossible, &ts); err != nil {
			return nil, err
		}
		s.PhotoPath = uploadPublicPath(s.PhotoPath)
		s.CreatedAt = time.Unix(ts, 0).Format("Jan 2 3:04 PM")
		out = append(out, s)
	}
	return out, rows.Err()
}

func uploadPublicPath(path string) string {
	path = filepath.ToSlash(strings.TrimSpace(path))
	path = strings.TrimPrefix(path, "./")
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "/uploads/") {
		return path
	}
	if strings.HasPrefix(path, "data/uploads/") {
		return "/uploads/" + strings.TrimPrefix(path, "data/uploads/")
	}
	return path
}

func (a *App) challengeSchedules() ([]ChallengeSchedule, error) {
	rows, err := a.db.Query(`SELECT cs.id, ch.username, c.title, cs.frequency, cs.next_run, cs.weekday, cs.active FROM challenge_schedules cs JOIN children ch ON ch.id=cs.child_id JOIN challenges c ON c.id=cs.challenge_id ORDER BY cs.active DESC, cs.next_run`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChallengeSchedule
	for rows.Next() {
		var s ChallengeSchedule
		var active int
		if err := rows.Scan(&s.ID, &s.ChildUsername, &s.ChallengeTitle, &s.Frequency, &s.NextRunUnix, &s.Weekday, &active); err != nil {
			return nil, err
		}
		s.Active = active == 1
		s.NextRun = time.Unix(s.NextRunUnix, 0).Format("Jan 2, 2006")
		out = append(out, s)
	}
	return out, rows.Err()
}

func (a *App) runDueSchedules() error {
	now := time.Now()
	rows, err := a.db.Query(`SELECT id, child_id, challenge_id, frequency, next_run, weekday FROM challenge_schedules WHERE active=1 AND next_run<=?`, startOfDay(now).Add(24*time.Hour).Add(-time.Second).Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	type due struct {
		id          int64
		childID     int64
		challengeID int64
		frequency   string
		nextRun     int64
		weekday     int
	}
	var schedules []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.childID, &d.challengeID, &d.frequency, &d.nextRun, &d.weekday); err != nil {
			return err
		}
		schedules = append(schedules, d)
	}
	for _, d := range schedules {
		_, _ = a.db.Exec(`INSERT INTO challenge_unlocks (child_id, challenge_id, created_at) VALUES (?, ?, ?)`, d.childID, d.challengeID, time.Now().Unix())
		next, active := nextRunAfter(d.frequency, time.Unix(d.nextRun, 0), d.weekday)
		if active {
			_, _ = a.db.Exec(`UPDATE challenge_schedules SET next_run=?, last_run=? WHERE id=?`, next, time.Now().Unix(), d.id)
		} else {
			_, _ = a.db.Exec(`UPDATE challenge_schedules SET active=0, last_run=? WHERE id=?`, time.Now().Unix(), d.id)
		}
	}
	return nil
}

func nextScheduleRun(frequency, dateText string, weekday int, now time.Time) (int64, int, error) {
	switch frequency {
	case "once":
		if dateText == "" {
			return 0, -1, errors.New("missing date")
		}
		t, err := time.ParseInLocation("2006-01-02", dateText, time.Local)
		if err != nil {
			return 0, -1, err
		}
		return startOfDay(t).Unix(), -1, nil
	case "daily":
		return startOfDay(now).Unix(), -1, nil
	case "weekly":
		if weekday < 0 || weekday > 6 {
			return 0, -1, errors.New("invalid weekday")
		}
		t := startOfDay(now)
		for int(t.Weekday()) != weekday {
			t = t.AddDate(0, 0, 1)
		}
		return t.Unix(), weekday, nil
	}
	return 0, -1, errors.New("invalid frequency")
}

func nextRunAfter(frequency string, current time.Time, weekday int) (int64, bool) {
	switch frequency {
	case "once":
		return 0, false
	case "daily":
		return startOfDay(current).AddDate(0, 0, 1).Unix(), true
	case "weekly":
		next := startOfDay(current).AddDate(0, 0, 7)
		if weekday >= 0 && weekday <= 6 {
			for int(next.Weekday()) != weekday {
				next = next.AddDate(0, 0, 1)
			}
		}
		return next.Unix(), true
	}
	return 0, false
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func (a *App) progress(childID int64) (int, int, int) {
	challenges, err := a.challenges()
	if err != nil {
		return 0, 0, 0
	}
	done := 0
	active := 0
	for _, c := range challenges {
		if _, _, _, _, ok := a.completedChallengeStatus(childID, c.ID); ok {
			done++
		}
		if a.challengeIsActive(childID, c.ID) {
			active++
		}
	}
	total := done + active
	percent := 0
	if total > 0 {
		percent = int(math.Round(float64(done) / float64(total) * 100))
	}
	return done, total, percent
}

func (a *App) childPoints(childID int64) int {
	var earned int
	_ = a.db.QueryRow(`SELECT COALESCE(SUM(points_awarded),0) FROM submissions WHERE child_id=? AND earns_points=1`, childID).Scan(&earned)
	var spent int
	_ = a.db.QueryRow(`SELECT COALESCE(SUM(points_spent),0) FROM reward_purchases WHERE child_id=?`, childID).Scan(&spent)
	var adjusted int
	_ = a.db.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM point_adjustments WHERE child_id=?`, childID).Scan(&adjusted)
	var strikes int
	_ = a.db.QueryRow(`SELECT COALESCE(SUM(points_deducted),0) FROM strike_events WHERE child_id=?`, childID).Scan(&strikes)
	return earned - spent + adjusted - strikes
}

func (a *App) strikeRules() ([]StrikeRule, error) {
	rows, err := a.db.Query(`SELECT id, title, description, points FROM strike_rules ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StrikeRule
	for rows.Next() {
		var s StrikeRule
		if err := rows.Scan(&s.ID, &s.Title, &s.Description, &s.Points); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (a *App) pointEvents(childID int64, limit int) ([]PointEvent, error) {
	var events []PointEvent
	add := func(e PointEvent) {
		e.CreatedAt = time.Unix(e.CreatedUnix, 0).Format("Jan 2 3:04 PM")
		events = append(events, e)
	}
	filter := ""
	args := []any{}
	if childID > 0 {
		filter = " WHERE ch.id=? AND s.earns_points=1"
		args = append(args, childID)
	} else {
		filter = " WHERE s.earns_points=1"
	}
	rows, err := a.db.Query(`SELECT ch.username, c.title, s.points_awarded, s.created_at FROM submissions s JOIN children ch ON ch.id=s.child_id JOIN challenges c ON c.id=s.challenge_id`+filter, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e PointEvent
		if err := rows.Scan(&e.ChildUsername, &e.Title, &e.Amount, &e.CreatedUnix); err != nil {
			_ = rows.Close()
			return nil, err
		}
		e.Kind = "Challenge"
		e.Detail = "Completed challenge"
		add(e)
	}
	_ = rows.Close()

	filter = ""
	args = []any{}
	if childID > 0 {
		filter = " WHERE ch.id=?"
		args = append(args, childID)
	}
	rows, err = a.db.Query(`SELECT ch.username, r.title, rp.points_spent, rp.created_at FROM reward_purchases rp JOIN children ch ON ch.id=rp.child_id JOIN rewards r ON r.id=rp.reward_id`+filter, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e PointEvent
		if err := rows.Scan(&e.ChildUsername, &e.Title, &e.Amount, &e.CreatedUnix); err != nil {
			_ = rows.Close()
			return nil, err
		}
		e.Kind = "Reward"
		e.Detail = "Redeemed by parent"
		e.Amount = -e.Amount
		add(e)
	}
	_ = rows.Close()

	filter = ""
	args = []any{}
	if childID > 0 {
		filter = " WHERE ch.id=?"
		args = append(args, childID)
	}
	rows, err = a.db.Query(`SELECT ch.username, pa.reason, pa.amount, pa.created_at FROM point_adjustments pa JOIN children ch ON ch.id=pa.child_id`+filter, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e PointEvent
		if err := rows.Scan(&e.ChildUsername, &e.Title, &e.Amount, &e.CreatedUnix); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if strings.HasPrefix(e.Title, "Gift") {
			e.Kind = "Gift"
			e.Detail = "Parent gift"
		} else {
			e.Kind = "Adjustment"
			e.Detail = "Parent adjustment"
		}
		add(e)
	}
	_ = rows.Close()

	filter = ""
	args = []any{}
	if childID > 0 {
		filter = " WHERE ch.id=?"
		args = append(args, childID)
	}
	rows, err = a.db.Query(`SELECT ch.username, se.title, se.description, se.points_deducted, se.created_at FROM strike_events se JOIN children ch ON ch.id=se.child_id`+filter, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e PointEvent
		if err := rows.Scan(&e.ChildUsername, &e.Title, &e.Detail, &e.Amount, &e.CreatedUnix); err != nil {
			_ = rows.Close()
			return nil, err
		}
		e.Kind = "Strike"
		e.Amount = -e.Amount
		add(e)
	}
	_ = rows.Close()

	sort.Slice(events, func(i, j int) bool {
		if events[i].CreatedUnix == events[j].CreatedUnix {
			return events[i].Title < events[j].Title
		}
		return events[i].CreatedUnix < events[j].CreatedUnix
	})
	balances := map[string]int{}
	for i := range events {
		balances[events[i].ChildUsername] += events[i].Amount
		events[i].Balance = balances[events[i].ChildUsername]
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].CreatedUnix == events[j].CreatedUnix {
			return events[i].Title > events[j].Title
		}
		return events[i].CreatedUnix > events[j].CreatedUnix
	})
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

func (a *App) challengeStatus(childID, challengeID int64) (string, int) {
	var status string
	var points int
	err := a.db.QueryRow(`SELECT status, points_awarded FROM submissions WHERE child_id=? AND challenge_id=? ORDER BY created_at DESC, id DESC LIMIT 1`, childID, challengeID).Scan(&status, &points)
	if err != nil {
		return "open", 0
	}
	if a.canEarnPoints(childID, challengeID) && status == "approved" {
		return "unlocked", points
	}
	return status, points
}

func (a *App) completedChallengeStatus(childID, challengeID int64) (string, int, string, string, bool) {
	var status string
	var points int
	var answer string
	var photoPath string
	var createdAt int64
	err := a.db.QueryRow(`SELECT status, points_awarded, answer, photo_path, created_at FROM submissions WHERE child_id=? AND challenge_id=? AND status IN ('approved','pending') ORDER BY created_at DESC, id DESC LIMIT 1`, childID, challengeID).Scan(&status, &points, &answer, &photoPath, &createdAt)
	if err != nil {
		return "", 0, "", "", false
	}
	challenge, err := a.challenge(challengeID)
	if err != nil {
		return "", 0, "", "", false
	}
	submitted := a.displayAnswer(challenge, answer)
	if photoPath != "" {
		submitted = "Photo submitted: " + photoPath
	}
	return status, points, submitted, time.Unix(createdAt, 0).Format("Jan 2 3:04 PM"), true
}

func (a *App) completedChallengeDetail(childID, challengeID int64) (ChallengeStatus, bool) {
	challenge, err := a.challenge(challengeID)
	if err != nil {
		return ChallengeStatus{}, false
	}
	status, earned, submitted, submittedAt, ok := a.completedChallengeStatus(childID, challengeID)
	if !ok {
		return ChallengeStatus{}, false
	}
	return ChallengeStatus{
		Challenge:     challenge,
		Status:        status,
		Earned:        earned,
		Submitted:     submitted,
		CorrectAnswer: a.correctAnswer(challenge),
		SubmittedAt:   submittedAt,
	}, true
}

func (a *App) challengeIsActive(childID, challengeID int64) bool {
	var unlockAt int64
	_ = a.db.QueryRow(`SELECT COALESCE(MAX(created_at),0) FROM challenge_unlocks WHERE child_id=? AND (challenge_id=? OR challenge_id IS NULL)`, childID, challengeID).Scan(&unlockAt)
	if unlockAt == 0 {
		return false
	}
	var completedAfterUnlock int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM submissions WHERE child_id=? AND challenge_id=? AND status IN ('approved','pending') AND created_at >= ?`, childID, challengeID, unlockAt).Scan(&completedAfterUnlock)
	return completedAfterUnlock == 0
}

func (a *App) displayAnswer(c Challenge, raw string) string {
	switch c.Type {
	case "multiple_choice", "true_false":
		for _, o := range c.Options {
			if strconv.FormatInt(o.ID, 10) == raw {
				return o.Text
			}
		}
	case "select_all":
		answers := []string{}
		selected := map[string]bool{}
		for _, id := range strings.Split(raw, ",") {
			selected[id] = true
		}
		for _, o := range c.Options {
			if selected[strconv.FormatInt(o.ID, 10)] {
				answers = append(answers, o.Text)
			}
		}
		if len(answers) > 0 {
			return strings.Join(answers, ", ")
		}
	}
	return raw
}

func (a *App) correctAnswer(c Challenge) string {
	switch c.Type {
	case "multiple_choice", "true_false", "select_all":
		answers := []string{}
		for _, o := range c.Options {
			if o.IsCorrect {
				answers = append(answers, o.Text)
			}
		}
		return strings.Join(answers, ", ")
	case "number", "short_answer":
		return c.Answer
	}
	return ""
}

func (a *App) canEarnPoints(childID, challengeID int64) bool {
	var unlockAt int64
	_ = a.db.QueryRow(`SELECT COALESCE(MAX(created_at),0) FROM challenge_unlocks WHERE child_id=? AND (challenge_id=? OR challenge_id IS NULL)`, childID, challengeID).Scan(&unlockAt)
	var count int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM submissions WHERE child_id=? AND challenge_id=? AND earns_points=1 AND created_at > ?`, childID, challengeID, unlockAt).Scan(&count)
	return count == 0
}

func (a *App) answerMatches(c Challenge, answer string) bool {
	switch c.Type {
	case "multiple_choice", "true_false":
		for _, o := range c.Options {
			if strconv.FormatInt(o.ID, 10) == answer {
				return o.IsCorrect
			}
		}
	case "select_all":
		want := map[string]bool{}
		for _, o := range c.Options {
			if o.IsCorrect {
				want[strconv.FormatInt(o.ID, 10)] = true
			}
		}
		got := map[string]bool{}
		for _, id := range strings.Split(answer, ",") {
			if id != "" {
				got[id] = true
			}
		}
		if len(want) != len(got) {
			return false
		}
		for id := range want {
			if !got[id] {
				return false
			}
		}
		return true
	case "number":
		want, err1 := strconv.ParseFloat(c.Answer, 64)
		got, err2 := strconv.ParseFloat(answer, 64)
		return err1 == nil && err2 == nil && math.Abs(want-got) < 0.000001
	default:
		return strings.EqualFold(strings.TrimSpace(c.Answer), strings.TrimSpace(answer))
	}
	return false
}

func (a *App) validChild(username, password string) (int64, bool) {
	var id int64
	var hash string
	err := a.db.QueryRow(`SELECT id, password_hash FROM children WHERE username=?`, username).Scan(&id, &hash)
	if err != nil {
		return 0, false
	}
	return id, hmac.Equal([]byte(hash), []byte(a.hashPassword(password)))
}

func (a *App) isBlocked(ip string) (bool, error) {
	if err := a.purgeFailures(); err != nil {
		return false, err
	}
	var count int
	err := a.db.QueryRow(`SELECT COUNT(*) FROM login_failures WHERE ip=? AND attempted_at >= ?`, ip, time.Now().Add(-failWindow).Unix()).Scan(&count)
	return count >= maxFailures, err
}

func (a *App) recordFailure(ip string) (bool, error) {
	if err := a.purgeFailures(); err != nil {
		return false, err
	}
	now := time.Now().Unix()
	if _, err := a.db.Exec(`INSERT INTO login_failures (ip, attempted_at) VALUES (?, ?)`, ip, now); err != nil {
		return false, err
	}
	var count int
	err := a.db.QueryRow(`SELECT COUNT(*) FROM login_failures WHERE ip=? AND attempted_at >= ?`, ip, time.Now().Add(-failWindow).Unix()).Scan(&count)
	return count >= maxFailures, err
}

func (a *App) purgeFailures() error {
	_, err := a.db.Exec(`DELETE FROM login_failures WHERE attempted_at < ?`, time.Now().Add(-failWindow).Unix())
	return err
}

func (a *App) startSession(w http.ResponseWriter, r *http.Request, role string, childID int64, redirect string) {
	session, err := a.sessions.Create(role, childID)
	if err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: a.signCookie(session.ID), Path: "/", Expires: session.ExpiresAt, MaxAge: int(sessionLifetime.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (a *App) currentSession(r *http.Request) (Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Session{}, false
	}
	id, ok := a.verifyCookie(c.Value)
	if !ok {
		return Session{}, false
	}
	return a.sessions.Get(id)
}

func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := a.currentSession(r)
		if !ok || session.Role != "admin" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *App) requireChild(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := a.currentSession(r)
		if !ok || session.Role != "child" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *App) signCookie(id string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte(id))
	return id + "." + hex.EncodeToString(mac.Sum(nil))
}

func (a *App) verifyCookie(value string) (string, bool) {
	id, sig, ok := strings.Cut(value, ".")
	if !ok || id == "" || sig == "" {
		return "", false
	}
	expected := a.signCookie(id)
	return id, hmac.Equal([]byte(value), []byte(expected))
}

func (a *App) hashPassword(password string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte(password))
	return hex.EncodeToString(mac.Sum(nil))
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func templates() *template.Template {
	return template.Must(template.New("base").Funcs(template.FuncMap{
		"eq": func(a, b any) bool { return a == b },
		"amountClass": func(v int) string {
			if v < 0 {
				return "delta-neg"
			}
			return "delta-pos"
		},
	}).Parse(html))
}

const html = `
{{define "layout_top"}}<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>iparent</title>
  <style>
    :root{font-family:Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;color:#18202f;background:#f3f6fb}
    body{margin:0;background:linear-gradient(180deg,#fff 0,#f2fbff 38%,#fff8fb 100%)}#rainbow-webgl{position:fixed;inset:0;width:100vw;height:100vh;z-index:-1;pointer-events:none;opacity:.45}.shell{max-width:1160px;margin:0 auto;padding:24px;position:relative}.top{display:flex;justify-content:space-between;align-items:center;gap:16px;margin-bottom:22px}.brand{font-weight:900;font-size:24px}.brand:before{content:"";display:inline-block;width:28px;height:14px;margin-right:8px;border-radius:28px 28px 0 0;background:linear-gradient(90deg,#ff5c77,#ffb23f,#ffe15a,#35c779,#34a7ff,#8b5cf6);vertical-align:middle}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:16px}.panel,.card{background:rgba(255,255,255,.94);border:1px solid #dbe2ec;border-radius:8px;padding:16px;box-shadow:0 8px 24px rgba(31,45,70,.06);backdrop-filter:blur(8px)}.panel h2,.card h3{margin:0 0 12px}.stack{display:grid;gap:10px}label{display:grid;gap:5px;font-size:14px;font-weight:650}input,select,textarea,button{font:inherit;border-radius:7px;border:1px solid #c8d2df;padding:10px;background:#fff}textarea{min-height:86px}button,.btn{background:linear-gradient(90deg,#ff5c77,#ffb23f,#35c779,#34a7ff,#8b5cf6);color:#fff;border:0;font-weight:850;cursor:pointer;text-decoration:none;display:inline-block}button:hover,.btn:hover{filter:brightness(.98);box-shadow:0 6px 18px rgba(52,167,255,.2)}.btn.secondary,button.secondary{background:#fff;color:#215cce;border:1px solid #cbd6e4}.muted{color:#657084}.error{background:#fff0f0;border:1px solid #e6b6b6;color:#9a1d1d;padding:10px;border-radius:7px}.notice{background:#edf8f0;border:1px solid #b9dfbd;color:#1f6b2a;padding:10px;border-radius:7px;margin-bottom:14px}.pill{display:inline-flex;align-items:center;gap:4px;padding:4px 9px;border-radius:999px;background:#e9edf4;font-size:12px;font-weight:800;white-space:nowrap}.nav-badge{display:inline-flex;align-items:center;justify-content:center;min-width:20px;height:20px;margin-left:6px;padding:0 6px;border-radius:999px;background:#ff5c77;color:#fff;font-size:12px;box-shadow:0 4px 12px rgba(255,92,119,.25)}.points{background:#ffe08a;color:#533800;border:1px solid #f2bf3d;box-shadow:inset 0 -1px 0 rgba(83,56,0,.12)}.points.big{font-size:28px;padding:10px 14px;border-radius:8px}.delta-pos{background:#e8f8ee;color:#17613a;border-color:#b8e4c7}.delta-neg{background:#fff0f0;color:#9a1d1d;border-color:#e6b6b6}.row{display:flex;align-items:center;justify-content:space-between;gap:12px}.progress{height:10px;background:#e5ebf3;border-radius:99px;overflow:hidden}.progress span{display:block;height:100%;background:linear-gradient(90deg,#ff5c77,#ffb23f,#ffe15a,#35c779,#34a7ff,#8b5cf6)}.challenge-form{border-top:1px solid #e5e9ef;margin-top:12px;padding-top:12px}.login{max-width:380px;margin:12vh auto}.login .brand{font-size:32px;margin-bottom:14px}.small{font-size:13px}.list{display:grid;gap:10px}.option{display:flex;align-items:center;gap:8px;font-weight:500}.option input{width:auto}.admin-form{align-content:start}.hero{background:linear-gradient(120deg,rgba(32,48,74,.96),rgba(47,111,188,.94) 45%,rgba(124,58,237,.94));color:#fff;border-color:#7c3aed}.hero .muted{color:#edf3ff}.todo{border-left:5px solid #34a7ff}.complete{background:rgba(251,252,253,.95);border:1px solid #e4eaf2;border-radius:8px;padding:12px}.strike{border-left:5px solid #d64545}.assign{display:grid;grid-template-columns:1fr auto;gap:8px}.empty{border:1px dashed #bfcad8;background:rgba(255,255,255,.7);padding:18px;border-radius:8px;text-align:center}.reward{background:rgba(255,250,240,.94);border-color:#f1d083}.child-nav{display:flex;gap:8px;flex-wrap:wrap;margin:-8px 0 18px}.child-nav a{background:rgba(255,255,255,.92);color:#215cce;border:1px solid #cbd6e4;border-radius:7px;padding:9px 11px;text-decoration:none;font-weight:800}.child-nav a:hover{border-color:#8b5cf6;box-shadow:0 4px 16px rgba(139,92,246,.15)}.answer{background:#f3f7fb;border:1px solid #d8e2ee;border-radius:8px;padding:10px}.answer strong{display:block;font-size:12px;text-transform:uppercase;color:#657084;margin-bottom:4px}.completed-link{color:inherit;text-decoration:none}.completed-link:hover{border-color:#215cce;box-shadow:0 8px 24px rgba(33,92,206,.12)}.detail-shell{max-width:760px}.back{margin-bottom:14px}.home-note{display:grid;grid-template-columns:minmax(0,1fr) 180px;gap:16px;align-items:center}.home-note img{width:180px;height:120px;object-fit:cover;border-radius:8px}.reward-img{width:100%;aspect-ratio:16/10;object-fit:cover;border-radius:8px;border:1px solid #ead9a8;background:#fff8df}.reward-pick-img{width:72px;height:72px;object-fit:cover;border-radius:8px;border:1px solid #ead9a8;background:#fff8df}.reward-thumb{width:54px;height:54px;object-fit:cover;border-radius:8px;border:1px solid #ead9a8}.review-photo{width:120px;height:90px;object-fit:cover;border-radius:8px;border:1px solid #d8e2ee;background:#f3f7fb;display:block}.review-photo-link{width:max-content;max-width:100%;display:block}.reward-list{display:flex;flex-wrap:wrap;gap:12px;margin-top:12px}.reward-card{width:190px;padding:12px}.reward-card .row{align-items:flex-start}.action-row{display:flex;justify-content:flex-start}.action-row button{width:auto;min-width:108px;padding:8px 10px}.event-kind{font-size:12px;text-transform:uppercase;font-weight:850;color:#657084}.ledger{display:grid;gap:8px}.ledger-head,.ledger-row{display:grid;grid-template-columns:minmax(0,1fr) 110px 110px;gap:12px;align-items:center}.ledger-head{padding:0 12px;color:#657084;font-size:12px;font-weight:850;text-transform:uppercase}.ledger-row{background:rgba(251,252,253,.95);border:1px solid #e4eaf2;border-radius:8px;padding:12px}.ledger-num{text-align:right;font-weight:850}@media(prefers-reduced-motion:reduce){#rainbow-webgl{display:none}}@media(max-width:640px){.shell{padding:16px}.top{align-items:flex-start}.assign,.row,.home-note{display:grid;justify-content:stretch}.ledger-head{display:none}.ledger-row{grid-template-columns:1fr}.ledger-num{text-align:left}.points.big{font-size:24px;text-align:center}.reward-card{width:150px}.home-note img{width:100%;height:160px}}
  </style>
</head><body><canvas id="rainbow-webgl" aria-hidden="true"></canvas><main class="shell">{{end}}
{{define "layout_bottom"}}</main><script>
(() => {
  if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
  const canvas = document.getElementById('rainbow-webgl');
  if (!canvas) return;
  const gl = canvas.getContext('webgl', { alpha: true, antialias: false, depth: false, stencil: false });
  if (!gl) return;
  const vertex = 'attribute vec2 p;void main(){gl_Position=vec4(p,0.0,1.0);}';
  const fragment = 'precision mediump float;uniform vec2 r;uniform float t;vec3 pal(float x){return .62+.38*cos(6.28318*(vec3(.00,.33,.67)+x));}void main(){vec2 uv=gl_FragCoord.xy/r;float y=uv.y+0.035*sin(uv.x*8.0+t*.45)+0.02*sin(uv.x*15.0-t*.22);float band=smoothstep(.18,.0,abs(y-.82));float glow=smoothstep(.55,.0,distance(uv,vec2(.18,.18)));float wash=.22+band*.55+glow*.12;vec3 c=pal(uv.x*.72+t*.035);gl_FragColor=vec4(c,wash*.26);}';
  function shader(type, source) {
    const s = gl.createShader(type);
    gl.shaderSource(s, source);
    gl.compileShader(s);
    return gl.getShaderParameter(s, gl.COMPILE_STATUS) ? s : null;
  }
  const vs = shader(gl.VERTEX_SHADER, vertex), fs = shader(gl.FRAGMENT_SHADER, fragment);
  if (!vs || !fs) return;
  const program = gl.createProgram();
  gl.attachShader(program, vs); gl.attachShader(program, fs); gl.linkProgram(program);
  if (!gl.getProgramParameter(program, gl.LINK_STATUS)) return;
  gl.useProgram(program);
  const buffer = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1,-1,1,-1,-1,1,-1,1,1,-1,1,1]), gl.STATIC_DRAW);
  const loc = gl.getAttribLocation(program, 'p');
  gl.enableVertexAttribArray(loc);
  gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0);
  const res = gl.getUniformLocation(program, 'r');
  const time = gl.getUniformLocation(program, 't');
  function resize() {
    const dpr = Math.min(window.devicePixelRatio || 1, 1.5);
    const w = Math.max(1, Math.floor(innerWidth * dpr));
    const h = Math.max(1, Math.floor(innerHeight * dpr));
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w; canvas.height = h;
      gl.viewport(0, 0, w, h);
    }
  }
  function draw(ms) {
    resize();
    gl.uniform2f(res, canvas.width, canvas.height);
    gl.uniform1f(time, ms * 0.001);
    gl.drawArrays(gl.TRIANGLES, 0, 6);
    requestAnimationFrame(draw);
  }
  requestAnimationFrame(draw);
})();
</script></body></html>{{end}}

{{define "login"}}{{template "layout_top" .}}
<section class="login panel">
  <div class="brand">iparent</div>
  <p class="muted">Sign in as the parent admin or as a child.</p>
  {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
  <form class="stack" method="post" action="/login">
    <label>Username <input name="username" autocomplete="username" required autofocus></label>
    <label>Password <input name="password" type="password" autocomplete="current-password" required></label>
    <button>Sign in</button>
  </form>
  <p class="muted small">Assets are ready. Sessions use a signed HTTP-only cookie.</p>
</section>
{{template "layout_bottom" .}}{{end}}

{{define "admin_top"}}
<div class="top"><div><div class="brand">iparent admin</div><div class="muted">Manage children, challenges, reviews, and rewards.</div></div><a class="btn secondary" href="/logout">Log out</a></div>
<nav class="child-nav"><a href="/admin/children">Children</a><a href="/admin/challenges">Challenges</a><a href="/admin/schedules">Schedules</a><a href="/admin/review">Review{{if .Pending}}<span class="nav-badge">{{len .Pending}}</span>{{end}}</a><a href="/admin/rewards">Rewards</a><a href="/admin/points">Points</a><a href="/admin/history">History</a><a href="/admin/strikes">Strikes</a></nav>
{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{end}}

{{define "admin_children"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<section class="grid">
  <section class="panel stack admin-form">
    <h2>Children</h2>
    {{range .Children}}<div class="card stack"><div class="row"><div><strong>{{if .Name}}{{.Name}}{{else}}{{.Username}}{{end}}</strong><div class="muted small">Username: {{.Username}}</div></div><span class="pill points">{{.Points}} pts</span></div><form class="assign" method="post" action="/admin/children/name"><input type="hidden" name="child_id" value="{{.ID}}"><input name="name" value="{{.Name}}" placeholder="Child's name" aria-label="Child's name" required><button class="secondary">Save name</button></form><div class="muted small">{{.Done}} / {{.Total}} complete · {{.Percent}}%</div><div class="progress"><span style="width:{{.Percent}}%"></span></div>{{if $.Challenges}}<form class="assign" method="post" action="/admin/reset"><input type="hidden" name="child_id" value="{{.ID}}"><select name="challenge_id" aria-label="Challenge to give">{{range $.Challenges}}<option value="{{.ID}}">{{.Title}} ({{.Points}} pts)</option>{{end}}</select><button>Give</button></form><form method="post" action="/admin/reset"><input type="hidden" name="child_id" value="{{.ID}}"><button class="secondary">Unlock all</button></form>{{else}}<div class="muted small">Create a challenge before assigning work.</div>{{end}}<form class="stack" method="post" action="/admin/children/home" enctype="multipart/form-data"><input type="hidden" name="child_id" value="{{.ID}}"><label>Homepage note <textarea name="message" placeholder="A note they will see when they sign in">{{.HomeMessage}}</textarea></label>{{if .HomeImagePath}}<img class="reward-thumb" src="{{.HomeImagePath}}" alt="">{{end}}<label>Homepage picture <input name="image" type="file" accept="image/*"></label><button class="secondary">Save homepage</button></form></div>{{else}}<p class="muted">No children yet.</p>{{end}}
  </section>
  <form class="panel stack admin-form" method="post" action="/admin/children">
    <h2>Add child</h2>
    <label>Name <input name="name" autocomplete="name" required></label>
    <label>Username <input name="username" required></label>
    <label>Password <input name="password" type="password" required></label>
    <button>Add child</button>
  </form>
</section>
{{template "layout_bottom" .}}{{end}}

{{define "admin_challenges"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<section class="grid">
  <form class="panel stack admin-form" method="post" action="/admin/challenges">
    <h2>Create challenge</h2>
    <label>Title <input name="title" required></label>
    <label>Prompt <textarea name="prompt" required></textarea></label>
    <label>Type <select name="type"><option value="multiple_choice">Multiple choice</option><option value="select_all">Select all that apply</option><option value="true_false">True / false</option><option value="number">Number</option><option value="short_answer">Short answer</option><option value="long_answer">Long answer</option><option value="photo">Photo proof</option></select></label>
    <label>Points <input name="points" type="number" min="1" value="10" required></label>
    <label>Options <textarea name="options" placeholder="Use one line per option. Prefix correct options with *"></textarea></label>
    <label>Exact answer <input name="answer" placeholder="For number or short answer"></label>
    <label class="option"><input type="checkbox" name="manual_grade"> Parent reviews before points are awarded</label>
    <button>Create challenge</button>
  </form>
  <section class="panel">
    <h2>Challenge bank</h2>
    <div class="list" style="margin-top:12px">{{range .Challenges}}<a class="card completed-link" href="/admin/challenges/{{.ID}}"><div class="row"><strong>{{.Title}}</strong><span class="pill">{{.Points}} pts</span></div><div class="muted">{{.Type}}</div><p>{{.Prompt}}</p><div class="muted small">Click to edit</div></a>{{else}}<p class="muted">No challenges yet.</p>{{end}}</div>
  </section>
</section>
{{template "layout_bottom" .}}{{end}}

{{define "admin_challenge_edit"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<div class="detail-shell"><a class="btn secondary back" href="/admin/challenges">Back to challenge bank</a><form class="panel stack admin-form" method="post" action="/admin/challenges/{{.EditChallenge.ID}}">
  <h2>Edit challenge</h2>
  <label>Title <input name="title" value="{{.EditChallenge.Title}}" required></label>
  <label>Prompt <textarea name="prompt" required>{{.EditChallenge.Prompt}}</textarea></label>
  <label>Type <select name="type"><option value="multiple_choice" {{if eq .EditChallenge.Type "multiple_choice"}}selected{{end}}>Multiple choice</option><option value="select_all" {{if eq .EditChallenge.Type "select_all"}}selected{{end}}>Select all that apply</option><option value="true_false" {{if eq .EditChallenge.Type "true_false"}}selected{{end}}>True / false</option><option value="number" {{if eq .EditChallenge.Type "number"}}selected{{end}}>Number</option><option value="short_answer" {{if eq .EditChallenge.Type "short_answer"}}selected{{end}}>Short answer</option><option value="long_answer" {{if eq .EditChallenge.Type "long_answer"}}selected{{end}}>Long answer</option><option value="photo" {{if eq .EditChallenge.Type "photo"}}selected{{end}}>Photo proof</option></select></label>
  <label>Points <input name="points" type="number" min="1" value="{{.EditChallenge.Points}}" required></label>
  <label>Options <textarea name="options" placeholder="Use one line per option. Prefix correct options with *">{{.EditChallenge.OptionLines}}</textarea></label>
  <label>Exact answer <input name="answer" value="{{.EditChallenge.Answer}}" placeholder="For number or short answer"></label>
  <label class="option"><input type="checkbox" name="manual_grade" {{if .EditChallenge.ManualGrade}}checked{{end}}> Parent reviews before points are awarded</label>
  <button>Save challenge</button>
</form></div>
{{template "layout_bottom" .}}{{end}}

{{define "admin_schedules"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<section class="grid">
  <form class="panel stack admin-form" method="post" action="/admin/schedules">
    <h2>Schedule challenge</h2>
    <label>Child <select name="child_id" required>{{range .Children}}<option value="{{.ID}}">{{.Username}}</option>{{end}}</select></label>
    <label>Challenge <select name="challenge_id" required>{{range .Challenges}}<option value="{{.ID}}">{{.Title}} ({{.Points}} pts)</option>{{end}}</select></label>
    <label>When <select name="frequency"><option value="once">On a specific date</option><option value="daily">Every day</option><option value="weekly">Every week</option></select></label>
    <label>Date <input name="date" type="date"></label>
    <label>Weekly day <select name="weekday"><option value="0">Sunday</option><option value="1">Monday</option><option value="2">Tuesday</option><option value="3">Wednesday</option><option value="4">Thursday</option><option value="5">Friday</option><option value="6">Saturday</option></select></label>
    <button>Create schedule</button>
  </form>
  <section class="panel">
    <h2>Scheduled challenges</h2>
    <div class="list" style="margin-top:12px">{{range .Schedules}}<div class="card"><div class="row"><strong>{{.ChildUsername}} · {{.ChallengeTitle}}</strong>{{if .Active}}<span class="pill points">{{.NextRun}}</span>{{else}}<span class="pill">Done</span>{{end}}</div><div class="muted small">{{.Frequency}}{{if eq .Frequency "weekly"}} · weekday {{.Weekday}}{{end}}</div></div>{{else}}<p class="muted">No schedules yet.</p>{{end}}</div>
  </section>
</section>
{{template "layout_bottom" .}}{{end}}

{{define "admin_rewards"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<section class="grid">
  <form class="panel stack admin-form" method="post" action="/admin/rewards" enctype="multipart/form-data">
    <h2>Add reward</h2>
    <label>Reward <input name="title" required></label>
    <label>Cost <input name="points" type="number" min="1" required></label>
    <label>Image <input name="image" type="file" accept="image/*"></label>
    <div class="muted small">Reward images must be 1 MB or smaller.</div>
    <button>Add reward</button>
  </form>
  <section class="panel">
    <h2>Rewards</h2>
    <div class="list" style="margin-top:12px">{{range .Rewards}}<a class="row card completed-link" href="/admin/rewards/{{.ID}}">{{if .ImagePath}}<img class="reward-thumb" src="{{.ImagePath}}" alt="">{{end}}<strong>{{.Title}}</strong><span class="pill points">{{.Points}} pts</span></a>{{else}}<p class="muted">No rewards yet.</p>{{end}}</div>
  </section>
  <form class="panel stack admin-form" method="post" action="/admin/rewards/redeem">
    <h2>Redeem for child</h2>
    <label>Child <select name="child_id" required>{{range .Children}}<option value="{{.ID}}">{{.Username}} ({{.Points}} pts)</option>{{end}}</select></label>
    <label>Reward <select name="reward_id" required>{{range .Rewards}}<option value="{{.ID}}">{{.Title}} ({{.Points}} pts)</option>{{end}}</select></label>
    <div class="muted small">When a parent redeems a reward, points are spent from the child's balance and the reward leaves that child's reward bank.</div>
    <button>Redeem reward</button>
  </form>
</section>
{{template "layout_bottom" .}}{{end}}

{{define "admin_reward_edit"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<div class="detail-shell"><a class="btn secondary back" href="/admin/rewards">Back to rewards</a><form class="panel stack admin-form" method="post" action="/admin/rewards/{{.EditReward.ID}}" enctype="multipart/form-data">
  <h2>Edit reward</h2>
  {{if .EditReward.ImagePath}}<img class="reward-img" src="{{.EditReward.ImagePath}}" alt="">{{end}}
  <label>Reward <input name="title" value="{{.EditReward.Title}}" required></label>
  <label>Cost <input name="points" type="number" min="1" value="{{.EditReward.Points}}" required></label>
  <label>Replace image <input name="image" type="file" accept="image/*"></label>
  <div class="muted small">Leave image blank to keep the current image. Reward images must be 1 MB or smaller.</div>
  <button>Save reward</button>
</form></div>
{{template "layout_bottom" .}}{{end}}

{{define "admin_review"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<section class="panel">
  <h2>Pending review</h2>
  <div class="list">
  {{range .Pending}}
    <form class="card stack" method="post" action="/admin/review">
      <input type="hidden" name="submission_id" value="{{.ID}}">
      <div class="row"><strong>{{.ChildUsername}} · {{.ChallengeTitle}}</strong><span class="pill">{{.PointsPossible}} pts</span></div>
      <div class="muted small">{{.CreatedAt}}</div>
      {{if .Answer}}<div>{{.Answer}}</div>{{end}}
      {{if .PhotoPath}}<a class="review-photo-link" href="{{.PhotoPath}}" target="_blank" rel="noopener"><img class="review-photo" src="{{.PhotoPath}}" alt="Submitted photo for {{.ChallengeTitle}}"></a>{{end}}
      <label>Credit percent <input name="credit" type="number" min="0" max="100" value="100"></label>
      <button>Save review</button>
    </form>
  {{else}}<p class="muted">Nothing needs review.</p>{{end}}
  </div>
</section>
{{template "layout_bottom" .}}{{end}}

{{define "admin_points"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<section class="grid">
  <form class="panel stack admin-form" method="post" action="/admin/points/gift">
    <h2>Gift points</h2>
    <label>Child <select name="child_id" required>{{range .Children}}<option value="{{.ID}}">{{.Username}} ({{.Points}} pts)</option>{{end}}</select></label>
    <label>Gift amount <input name="amount" type="number" min="1" step="1" required></label>
    <label>Message <input name="reason" placeholder="Nice work today"></label>
    <button>Gift points</button>
  </form>
  <form class="panel stack admin-form" method="post" action="/admin/points/adjust">
    <h2>Adjust points</h2>
    <label>Child <select name="child_id" required>{{range .Children}}<option value="{{.ID}}">{{.Username}} ({{.Points}} pts)</option>{{end}}</select></label>
    <label>Amount <input name="amount" type="number" step="1" placeholder="Use negative numbers to remove points" required></label>
    <label>Reason <input name="reason" placeholder="Why are points changing?"></label>
    <button>Save adjustment</button>
  </form>
  <section class="panel">
    <h2>Point balances</h2>
    <div class="list" style="margin-top:12px">{{range .Children}}<div class="row complete"><strong>{{.Username}}</strong><span class="pill points">{{.Points}} pts</span></div>{{else}}<p class="muted">No children yet.</p>{{end}}</div>
  </section>
</section>
{{template "layout_bottom" .}}{{end}}

{{define "admin_history"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<section class="panel hero"><div class="row"><div><h2>History ledger</h2><div class="muted">Challenges, gifts, adjustments, rewards, and strikes in one place.</div></div><span class="pill points">{{len .Events}} entries</span></div></section>
<section class="panel" style="margin-top:16px"><div class="ledger"><div class="ledger-head"><div>Transaction</div><div class="ledger-num">Change</div><div class="ledger-num">Balance</div></div>{{range .Events}}<div class="ledger-row"><div><div class="event-kind">{{.Kind}} · {{.ChildUsername}}</div><strong>{{.Title}}</strong>{{if .Detail}}<div class="muted small">{{.Detail}}</div>{{end}}<div class="muted small">{{.CreatedAt}}</div></div><div class="ledger-num"><span class="pill {{amountClass .Amount}}">{{printf "%+d" .Amount}} pts</span></div><div class="ledger-num"><span class="pill points">{{.Balance}} pts</span></div></div>{{else}}<p class="muted">History will show up here.</p>{{end}}</div></section>
{{template "layout_bottom" .}}{{end}}

{{define "admin_strikes"}}{{template "layout_top" .}}
{{template "admin_top" .}}
<section class="grid">
  <form class="panel stack admin-form" method="post" action="/admin/strikes">
    <h2>Create strike</h2>
    <label>Name <input name="title" placeholder="Example: Screen sneaking" required></label>
    <label>Point deduction <input name="points" type="number" min="1" value="5" required></label>
    <label>What it means <textarea name="description" placeholder="Describe the behavior clearly."></textarea></label>
    <button>Create strike</button>
  </form>
  <form class="panel stack admin-form" method="post" action="/admin/strikes/impose">
    <h2>Apply strike</h2>
    <label>Child <select name="child_id" required>{{range .Children}}<option value="{{.ID}}">{{.Username}} ({{.Points}} pts)</option>{{end}}</select></label>
    <label>Strike <select name="strike_id" required>{{range .Strikes}}<option value="{{.ID}}">{{.Title}} (-{{.Points}} pts)</option>{{end}}</select></label>
    <button>Apply strike</button>
  </form>
</section>
<section class="panel" style="margin-top:16px"><h2>Strike list</h2><div class="list" style="margin-top:12px">{{range .Strikes}}<div class="card strike"><div class="row"><strong>{{.Title}}</strong><span class="pill delta-neg">-{{.Points}} pts</span></div>{{if .Description}}<p>{{.Description}}</p>{{end}}</div>{{else}}<p class="muted">No strikes created yet.</p>{{end}}</div></section>
<section class="panel" style="margin-top:16px"><h2>Recent point history</h2><div class="ledger" style="margin-top:12px"><div class="ledger-head"><div>Transaction</div><div class="ledger-num">Change</div><div class="ledger-num">Balance</div></div>{{range .Events}}<div class="ledger-row"><div><div class="event-kind">{{.Kind}} · {{.ChildUsername}}</div><strong>{{.Title}}</strong>{{if .Detail}}<div class="muted small">{{.Detail}}</div>{{end}}<div class="muted small">{{.CreatedAt}}</div></div><div class="ledger-num"><span class="pill {{amountClass .Amount}}">{{printf "%+d" .Amount}} pts</span></div><div class="ledger-num"><span class="pill points">{{.Balance}} pts</span></div></div>{{else}}<p class="muted">History will show up here.</p>{{end}}</div></section>
{{template "layout_bottom" .}}{{end}}

{{define "child_top"}}
<div class="top"><div><div class="brand">Hi, {{if .Child.Name}}{{.Child.Name}}{{else}}{{.Child.Username}}{{end}}</div><div class="muted">{{.Child.Done}} of {{.Child.Total}} assigned challenges complete</div></div><div class="row"><span class="pill points big">{{.Child.Points}} pts</span><a class="btn secondary" href="/logout">Log out</a></div></div>
<nav class="child-nav"><a href="/child">Open challenges</a><a href="/child/completed">Completed</a><a href="/child/rewards">Rewards</a><a href="/child/history">History</a></nav>
{{if .Message}}<div class="notice">{{.Message}}</div>{{end}}
{{end}}

{{define "child_challenges"}}{{template "layout_top" .}}
{{template "child_top" .}}
{{if or .Child.HomeMessage .Child.HomeImagePath}}<section class="panel home-note" style="margin-bottom:16px"><div><h2>From your parent</h2>{{if .Child.HomeMessage}}<p>{{.Child.HomeMessage}}</p>{{else}}<p class="muted">A picture was left for you today.</p>{{end}}</div>{{if .Child.HomeImagePath}}<img src="{{.Child.HomeImagePath}}" alt="">{{end}}</section>{{end}}
<section class="panel hero"><div class="row"><div><h2>Open challenges</h2><div class="muted">{{.Child.Percent}}% complete</div></div><span class="pill points">{{.Child.Points}} points ready</span></div><div class="progress" style="margin-top:12px"><span style="width:{{.Child.Percent}}%"></span></div></section>
<section class="grid" style="margin-top:16px">
  {{range .Challenges}}
  <article class="card todo">
    <div class="row"><h3>{{.Title}}</h3><span class="pill">{{.Points}} pts</span></div>
    <p>{{.Prompt}}</p>
    <div class="muted small">Status: {{.Status}}{{if .Earned}} · earned {{.Earned}}{{end}}</div>
    <form class="challenge-form stack" method="post" action="/child/submit" enctype="multipart/form-data">
      <input type="hidden" name="challenge_id" value="{{.ID}}">
      {{if eq .Type "multiple_choice"}}{{range .Options}}<label class="option"><input type="radio" name="answer" value="{{.ID}}" required>{{.Text}}</label>{{end}}{{end}}
      {{if eq .Type "true_false"}}{{range .Options}}<label class="option"><input type="radio" name="answer" value="{{.ID}}" required>{{.Text}}</label>{{end}}{{end}}
      {{if eq .Type "select_all"}}{{range .Options}}<label class="option"><input type="checkbox" name="answer" value="{{.ID}}">{{.Text}}</label>{{end}}{{end}}
      {{if eq .Type "number"}}<label>Answer <input name="answer" type="number" step="any" required></label>{{end}}
      {{if eq .Type "short_answer"}}<label>Answer <input name="answer" required></label>{{end}}
      {{if eq .Type "long_answer"}}<label>Answer <textarea name="answer" required></textarea></label>{{end}}
      {{if eq .Type "photo"}}<label>Photo <input name="photo" type="file" accept="image/*" required></label>{{end}}
      <button>Submit</button>
    </form>
  </article>
  {{else}}<div class="empty">No challenges are ready right now.</div>{{end}}
</section>
{{template "layout_bottom" .}}{{end}}

{{define "child_completed"}}{{template "layout_top" .}}
{{template "child_top" .}}
<section class="panel"><div class="row"><div><h2>Completed challenges</h2><div class="muted small">Open one to review the question and answer.</div></div><span class="pill">{{len .CompletedChallenges}} done</span></div><div class="list" style="margin-top:12px">{{range .CompletedChallenges}}<a class="complete row completed-link" href="/child/completed/{{.ID}}"><div><strong>{{.Title}}</strong><div class="muted small">{{.SubmittedAt}} · {{.Status}}{{if .Earned}} · earned {{.Earned}} pts{{end}}</div></div><span class="pill points">{{.Points}} pts</span></a>{{else}}<p class="muted">Completed challenges will show up here.</p>{{end}}</div></section>
{{template "layout_bottom" .}}{{end}}

{{define "child_completed_detail"}}{{template "layout_top" .}}
{{template "child_top" .}}
<div class="detail-shell"><a class="btn secondary back" href="/child/completed">Back to completed</a><article class="panel stack"><div class="row"><div><h2>{{.CompletedDetail.Title}}</h2><div class="muted small">{{.CompletedDetail.SubmittedAt}} · {{.CompletedDetail.Status}}{{if .CompletedDetail.Earned}} · earned {{.CompletedDetail.Earned}} pts{{end}}</div></div><span class="pill points">{{.CompletedDetail.Points}} pts</span></div><div><strong>Question</strong><p>{{.CompletedDetail.Prompt}}</p></div><div class="answer"><strong>Your answer</strong>{{if .CompletedDetail.Submitted}}{{.CompletedDetail.Submitted}}{{else}}No answer text saved.{{end}}</div>{{if .CompletedDetail.CorrectAnswer}}<div class="answer"><strong>Correct answer</strong>{{.CompletedDetail.CorrectAnswer}}</div>{{end}}</article></div>
{{template "layout_bottom" .}}{{end}}

{{define "child_rewards"}}{{template "layout_top" .}}
{{template "child_top" .}}
<section class="panel reward"><div class="row"><div><h2>Reward bank</h2><div class="muted small">Tell your parent when you want to redeem one.</div></div><span class="pill points">{{.Child.Points}} pts available</span></div><div class="reward-list">{{range .Rewards}}<div class="card stack reward-card">{{if .ImagePath}}<img class="reward-pick-img" src="{{.ImagePath}}" alt="">{{end}}<div class="row"><strong>{{.Title}}</strong><span class="pill points">{{.Points}} pts</span></div></div>{{else}}<p class="muted">No rewards are waiting right now.</p>{{end}}</div></section>
<section class="panel" style="margin-top:16px"><h2>Redeemed rewards</h2><div class="list">{{range .Purchases}}<div class="row complete"><div><strong>{{.Title}}</strong><div class="muted small">{{.CreatedAt}}</div></div><span class="pill">{{.Points}} pts</span></div>{{else}}<p class="muted">Rewards your parent redeems will show up here.</p>{{end}}</div></section>
{{template "layout_bottom" .}}{{end}}

{{define "child_history"}}{{template "layout_top" .}}
{{template "child_top" .}}
<section class="panel hero"><div class="row"><div><h2>History ledger</h2><div class="muted">Every point earned, gifted, spent, adjusted, or lost.</div></div><span class="pill points">{{.Child.Points}} pts now</span></div></section>
<section class="panel" style="margin-top:16px"><div class="ledger"><div class="ledger-head"><div>Transaction</div><div class="ledger-num">Change</div><div class="ledger-num">Balance</div></div>{{range .Events}}<div class="ledger-row"><div><div class="event-kind">{{.Kind}}</div><strong>{{.Title}}</strong>{{if .Detail}}<div class="muted small">{{.Detail}}</div>{{end}}<div class="muted small">{{.CreatedAt}}</div></div><div class="ledger-num"><span class="pill {{amountClass .Amount}}">{{printf "%+d" .Amount}} pts</span></div><div class="ledger-num"><span class="pill points">{{.Balance}} pts</span></div></div>{{else}}<p class="muted">Your point history will show up here.</p>{{end}}</div></section>
<section class="panel" style="margin-top:16px"><h2>Things that can cause strikes</h2><div class="list" style="margin-top:12px">{{range .Strikes}}<div class="card strike"><div class="row"><strong>{{.Title}}</strong><span class="pill delta-neg">-{{.Points}} pts</span></div>{{if .Description}}<p>{{.Description}}</p>{{end}}</div>{{else}}<p class="muted">No strikes are set up.</p>{{end}}</div></section>
{{template "layout_bottom" .}}{{end}}
`
