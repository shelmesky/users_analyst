package main

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	SHOW_LOCKS             = 20
	SERVER_LISTEN          = "0.0.0.0:34567"
	SERVER_PERF_LISTEN     = "0.0.0.0:34568"
	ENABLE_PERF_PROFILE    = true
	DELAY_SECONDS          = 10
	LOG_FILE               = "server.log"
	REPORT_SERVER_ADDRESS  = "http://10.252.147.206:9999"
	REPORT_SERVER_PUSH_URL = "/v1/live_show_update_attend"
)

var (
	logger                *log.Logger
	allShow               *AllShow
	signal_chan           chan os.Signal // 处理信号的channel
	showTimer             *ShowTimer
	show_client_data_pool *sync.Pool
	h5_user_pool          *sync.Pool
)

// POST到分析服务的结构
type ShowStatus struct {
	ShowID      string `json:"show_id"`
	AttendTotal uint   `json:"attend_total"`
	Channel     int64  `json:"channel"`
}

// 保存每个Show启动的定时器
type ShowTimer struct {
	Timers map[string]bool
	Lock   *sync.RWMutex
}

// H5用户
type H5User struct {
	LastUpdate int64
}

// 每个Show
type Show struct {
	ShowID       string
	Count        uint
	H5CookieUser map[string]*H5User
	UsersLocks   []*sync.RWMutex
}

// 所有的Show
type AllShow struct {
	Shows map[string]*Show
	Lock  *sync.RWMutex
}

// H5页面POST来的数据
type ShowClientData struct {
	ShowID string `json:"show_id"`
}

func init() {
	// init logging
	log_file, err := os.OpenFile(LOG_FILE, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
	logger = log.New(log_file, "Server: ", log.Ldate|log.Ltime|log.Lshortfile)
	ExtraInit()
}

func ExtraInit() {
	allShow = new(AllShow)
	allShow.Shows = make(map[string]*Show, 0)
	allShow.Lock = new(sync.RWMutex)

	showTimer = new(ShowTimer)
	showTimer.Timers = make(map[string]bool, 0)
	showTimer.Lock = new(sync.RWMutex)

	show_client_data_pool = &sync.Pool{
		New: func() interface{} {
			return new(ShowClientData)
		},
	}

	h5_user_pool = &sync.Pool{
		New: func() interface{} {
			return new(H5User)
		},
	}
}

// 计算字符换的MD5
func MD5(text string) string {
	hashMD5 := md5.New()
	io.WriteString(hashMD5, text)
	return fmt.Sprintf("%x", hashMD5.Sum(nil))
}

// 创建新的Show结构体
func NewShow(show_id string) *Show {
	var users_locks []*sync.RWMutex

	show := new(Show)
	show.ShowID = show_id
	show.H5CookieUser = make(map[string]*H5User, 0)

	for i := 0; i < SHOW_LOCKS; i++ {
		lock := new(sync.RWMutex)
		users_locks = append(users_locks, lock)
	}
	show.UsersLocks = users_locks

	return show
}

// 根据cookie内容取模获取对应的读写锁
func GetUserLock(show_id string, cookie_str string) (*sync.RWMutex, error) {
	defer func() {
		if err := recover(); err != nil {
			logger.Println(err)
		}
	}()

	var show *Show
	var ok bool

	// 根据cookie的值取得锁id
	cookie_int, err := strconv.Atoi(cookie_str)
	if err != nil {
		logger.Println("failed convert show_id to integer")
		return nil, fmt.Errorf("failed convert show_id to integer\n")
	}

	lock_id := cookie_int % SHOW_LOCKS

	// 根据show_id取得Show
	allShow.Lock.RLock()
	if show, ok = allShow.Shows[show_id]; !ok {
		logger.Println("can not find lock with show_id: ", show_id)
		return nil, fmt.Errorf("can not find lock with show_id: %s", show_id)
	}
	allShow.Lock.RUnlock()

	// 用lock_id在Show的锁列表中取得锁
	return show.UsersLocks[lock_id], nil
}

// 接收H5页面的/api/live_show请求
func ShowIDHandler(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if err := recover(); err != nil {
			logger.Println(err)
		}
	}()

	var empty_cookie bool
	var cookie_str string
	var h5_user *H5User
	var show *Show
	var ok bool

	w.Header().Set("Access-Control-Allow-Origin", "http://a.bolo.me")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "X-Requested-With")
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	empty_cookie = false
	cookie, err := r.Cookie("h5_user")

	if err == nil {
		cookie_str = strings.Trim(cookie.Value, " ")
		if cookie_str == "" {
			empty_cookie = true
		}
	}

	// 如果cookie为空，则为用户设置cookie并返回
	// cookie值为纳秒时间，一秒等于一千万纳秒
	if err != nil || empty_cookie == true {
		now := time.Now()
		now_nano := int(now.UnixNano())
		expiration := now.AddDate(3, 0, 0)
		cookie_value := strconv.Itoa(now_nano)
		cookie := http.Cookie{Name: "h5_user", Value: cookie_value, Expires: expiration, MaxAge: 50000, Path: "/"}
		http.SetCookie(w, &cookie)
		logger.Printf("Set cookie for %s -> %s\n", r.RemoteAddr, cookie_value)
		return
	}

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		logger.Println(err)
		http.Error(w, "read post body err", 500)
		return
	}

	if len(data) == 0 {
		logger.Println("Invalid data from cache client")
		http.Error(w, "post body is empty", 404)
		return
	}

	show_client_data := show_client_data_pool.Get().(*ShowClientData)

	err = json.Unmarshal(data, show_client_data)
	if err != nil {
		logger.Printf("Unmarshal JSON err: %s, data: %s\n", err, data)
		http.Error(w, "json unmarshal error", 500)
		show_client_data_pool.Put(show_client_data)
		return
	}

	show_client_data.ShowID = strings.Trim(show_client_data.ShowID, " ")
	if show_client_data.ShowID == "" {
		logger.Println("show id is empty\n")
		//http.Error(w, "show id is empty", 500)
		return
	}

	allShow.Lock.RLock()
	show, ok = allShow.Shows[show_client_data.ShowID]
	allShow.Lock.RUnlock()

	if ok {

		user_lock, err := GetUserLock(show_client_data.ShowID, cookie_str)
		if err != nil {
			logger.Println(err)
			http.Error(w, err.Error(), 500)
			show_client_data_pool.Put(show_client_data)
			return
		}

		user_lock.RLock()
		if h5_user, ok = show.H5CookieUser[cookie_str]; ok {
			//logger.Println("got h5_user:", h5_user)
			h5_user.LastUpdate = time.Now().Unix()
		}
		user_lock.RUnlock()

		if !ok {
			new_h5_user := new(H5User)
			new_h5_user.LastUpdate = time.Now().Unix()
			user_lock.Lock()
			logger.Println("make new h5_user:", new_h5_user, "cookie:", cookie_str)
			if show != nil {
				if show.H5CookieUser != nil {
					show.H5CookieUser[cookie_str] = new_h5_user
				}
			}
			user_lock.Unlock()
		}

	} else {
		show := NewShow(show_client_data.ShowID)
		allShow.Lock.Lock()
		logger.Printf("show_id %s is nil, make it\n", show_client_data.ShowID)
		allShow.Shows[show_client_data.ShowID] = show
		allShow.Lock.Unlock()

		showTimer.Lock.Lock()
		exists, ok := showTimer.Timers[show_client_data.ShowID]
		if !ok || !exists {
			go ShowTimeTicker(show_client_data.ShowID)
			showTimer.Timers[show_client_data.ShowID] = true
		}
		showTimer.Lock.Unlock()
	}

	show_client_data_pool.Put(show_client_data)
}

// 为每个Show启动定时器
func ShowTimeTicker(show_id string) {
	defer func() {
		if err := recover(); err != nil {
			logger.Println(err)
		}
	}()

	var show *Show
	var ok bool
	var show_status ShowStatus

	logger.Println("Start timer for show:", show_id)

	// 首先暂停5秒，等待用户的请求
	time.Sleep(10 * time.Second)

	c := time.Tick(1 * time.Second)

	for _ = range c {
		allShow.Lock.RLock()
		show, ok = allShow.Shows[show_id]
		if !ok {
			logger.Printf("Can not find show: %s\n", show_id)
		}
		allShow.Lock.RUnlock()

		var i uint = 0
		now := time.Now().Unix()

		if show != nil {
			for key := range show.H5CookieUser {
				user := show.H5CookieUser[key]
				if now-user.LastUpdate < DELAY_SECONDS {
					i += 1
				} else {
					user_lock, err := GetUserLock(show_id, key)
					if err != nil {
						logger.Println("Got user lock failed:", err)
					}
					user_lock.Lock()
					delete(show.H5CookieUser, key)
					user_lock.Unlock()
					logger.Printf("User [%s] has offline.\n", key)
				}
			}
		}

		// Show有人观看
		if i > 0 {
			show.Count = i
			logger.Printf("Show [%s] online [%d]\n", show_id, i)

			show_status.AttendTotal = i
			show_status.Channel = 1
			show_status.ShowID = show_id

			buf, err := json.Marshal(show_status)
			if err != nil {
				logger.Println("Json marshal failed:", err)
				continue
			}

			buf_reader := bytes.NewReader(buf)
			resp, err := http.Post(REPORT_SERVER_ADDRESS+REPORT_SERVER_PUSH_URL, "application/json", buf_reader)

			if err != nil {
				logger.Println("POST data to Push server failed:", err)
				continue
			}

			if resp != nil {
				resp.Body.Close()
			}
			logger.Println("Got response from report server:", resp.Status)
		}

		// Show无人观看
		// 应该退出定时器并清除Show相关的资源
		if i == 0 {
			// 删除Show
			if _, ok := allShow.Shows[show_id]; ok {
				delete(allShow.Shows, show_id)
			}

			// 删除Show的定时器状态
			showTimer.Lock.Lock()
			delete(showTimer.Timers, show_id)
			showTimer.Lock.Unlock()

			logger.Printf("Show [%s] has offline, clear resource...\n", show_id)
			return
		}
	}
}

// 信号回调
func signalCallback() {
	for s := range signal_chan {
		sig := s.String()
		logger.Println("Got Signal: " + sig)

		if s == syscall.SIGINT || s == syscall.SIGTERM {
			logger.Println("Server exit...")
			os.Exit(0)
		}
	}
}

func GC() {
	for {
		logger.Println("Start GC now...")
		runtime.GC()
		logger.Println("End GC now...")
		time.Sleep(60 * time.Second)
	}
}

func main() {
	defer func() {
		if err := recover(); err != nil {
			logger.Println(err)
			debug.PrintStack()
		}
	}()

	runtime.GOMAXPROCS(runtime.NumCPU())

	// 每60秒GC一次
	go GC()

	// HOLD住POSIX SIGNAL
	signal_chan = make(chan os.Signal, 10)
	signal.Notify(signal_chan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGPIPE,
		syscall.SIGALRM,
		syscall.SIGPIPE)

	go signalCallback()

	// 启动性能调试接口
	if ENABLE_PERF_PROFILE == true {
		go func() {
			http.ListenAndServe(SERVER_PERF_LISTEN, nil)
		}()
	}

	http.HandleFunc("/api/live_show", ShowIDHandler)

	s := &http.Server{
		Addr:           SERVER_LISTEN,
		Handler:        nil,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	s.SetKeepAlivesEnabled(false)

	logger.Printf("Server [PID: %d] listen on [%s]\n", os.Getpid(), SERVER_LISTEN)
	logger.Fatal(s.ListenAndServe())
}
