package conf

import (
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wweir/sower/internal/http"
	"github.com/wweir/sower/util"
	"github.com/wweir/utils/log"
	"github.com/wweir/utils/mem"
)

type dynamic struct {
	port http.Port
}

var cache = mem.New(2 * time.Hour)
var detect = &dynamic{}
var passwordData []byte
var timeout time.Duration

// ShouldProxy check if the domain shoule request though proxy
func ShouldProxy(domain string) bool {
	if Client.Router.DirectRules.Match(domain) {
		return false
	}
	if Client.Router.ProxyRules.Match(domain) {
		return true
	}
	if Client.Router.DynamicRules.Match(domain) {
		return true
	}

	cache.Remember(detect, domain)
	return Client.Router.DynamicRules.Match(domain)
}

func (d *dynamic) Get(key interface{}) (err error) {
	// break deadloop, for ugly wildcard setting dns setting
	domain := strings.TrimSuffix(key.(string), ".")
	if strings.Count(domain, ".") > 10 {
		return nil
	}

	wg := sync.WaitGroup{}
	httpScore, httpsScore := new(int32), new(int32)
	for _, ping := range [...]dynamic{{port: http.HTTP}, {port: http.HTTPS}} {
		wg.Add(1)
		go func(ping dynamic) {
			defer wg.Done()

			if err := ping.port.Ping(domain, timeout); err != nil {
				return
			}

			switch ping.port {
			case http.HTTP:
				if !atomic.CompareAndSwapInt32(httpScore, 0, 2) {
					atomic.AddInt32(httpScore, 1)
				}
			case http.HTTPS:
				if !atomic.CompareAndSwapInt32(httpsScore, 0, 2) {
					atomic.AddInt32(httpScore, 1)
				}
			}
		}(ping)
	}
	for _, ping := range [...]dynamic{{port: http.HTTP}, {port: http.HTTPS}} {
		wg.Add(1)
		go func(ping dynamic) {
			defer wg.Done()

			var conn net.Conn
			if conn, err = tls.Dial("tcp", net.JoinHostPort(Client.Address, "443"), &tls.Config{}); err != nil {
				log.Errorw("tls dial", "addr", net.JoinHostPort(Client.Address, "443"), "err", err)
				return
			}

			if ping.port == http.HTTP {
				conn = http.NewTgtConn(conn, passwordData, http.TGT_HTTP, "", 80)
			} else {
				conn = http.NewTgtConn(conn, passwordData, http.TGT_HTTPS, "", 443)
			}

			if err := ping.port.PingWithConn(domain, conn, timeout); err != nil {
				return
			}

			switch ping.port {
			case http.HTTP:
				if !atomic.CompareAndSwapInt32(httpScore, 0, -2) {
					atomic.AddInt32(httpScore, -1)
				}
			case http.HTTPS:
				if !atomic.CompareAndSwapInt32(httpsScore, 0, -2) {
					atomic.AddInt32(httpScore, -1)
				}
			}
		}(ping)
	}

	wg.Wait()
	if int(*httpScore+*httpsScore)+conf.Client.Router.DetectLevel < 0 {
		addDynamic(domain)
		log.Infow("add rule", "domain", domain, "http_score", *httpScore, "https_score", *httpsScore)
	}
	return nil
}

// addDynamic add new domain into dynamic list
func addDynamic(domain string) {
	flushMu.Lock()
	Client.Router.DynamicList = util.NewReverseSecSlice(
		append(Client.Router.DynamicList, domain)).Sort().Uniq()
	Client.Router.DynamicRules = util.NewNodeFromRules(Client.Router.DynamicList...)
	flushMu.Unlock()

	flushOnce.Do(func() {
		if conf.file != "" {
			go flushConf()
		}
	})

	select {
	case flushCh <- struct{}{}:
	default:
	}
}
