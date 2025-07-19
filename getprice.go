package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

type BitgetWSClient struct {
	conn              *websocket.Conn
	apiKey            string
	secretKey         string
	passphrase        string
	reconnectInterval time.Duration
	proxyUrl          string
	isAuthenticated   bool
	subscriptions     []map[string]string
	pingTicker        *time.Ticker
	done              chan struct{}
}

func NewBitgetWSClient(apiKey, secretKey, passphrase, proxyUrl string, reconnectInterval time.Duration) *BitgetWSClient {
	return &BitgetWSClient{
		reconnectInterval: reconnectInterval,
		apiKey:            apiKey,
		secretKey:         secretKey,
		passphrase:        passphrase,
		proxyUrl:          proxyUrl,
		subscriptions:     make([]map[string]string, 0),
		done:              make(chan struct{}),
	}
}

func (c *BitgetWSClient) Connect() error {
	// 设置代理
	proxy, _ := url.Parse(c.proxyUrl)
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxy),
	}

	dialer := websocket.DefaultDialer
	dialer.Proxy = transport.Proxy

	conn, _, err := dialer.Dial("wss://ws.bitget.com/v2/ws/public", nil)
	if err != nil {
		return err
	}
	c.conn = conn

	// 启动心跳
	c.startPingPong()

	// 处理认证
	if c.apiKey != "" && c.secretKey != "" {
		c.authenticate()
	} else {
		c.subscribeToChannels()
	}

	// 启动消息处理
	go c.handleMessages()
	return nil
}

func (c *BitgetWSClient) startPingPong() {
	c.pingTicker = time.NewTicker(30 * time.Second)
	go func() {
		for {
			select {
			case <-c.pingTicker.C:
				c.sendMessage("ping")
			case <-c.done:
				return
			}
		}
	}()
}

func (c *BitgetWSClient) authenticate() {
	timestamp := time.Now().Unix()
	message := fmt.Sprintf("%dGET/user/verify", timestamp)

	h := hmac.New(sha256.New, []byte(c.secretKey))
	h.Write([]byte(message))
	sign := base64.StdEncoding.EncodeToString(h.Sum(nil))

	loginMsg := map[string]interface{}{
		"op": "login",
		"args": []map[string]string{
			{
				"apiKey":     c.apiKey,
				"passphrase": c.passphrase,
				"timestamp":  fmt.Sprintf("%d", timestamp),
				"sign":       sign,
			},
		},
	}
	c.sendMessage(loginMsg)
}

func (c *BitgetWSClient) Subscribe(instType, channel, instId string) {
	sub := map[string]string{
		"instType": instType,
		"channel":  channel,
		"instId":   instId,
	}
	c.subscriptions = append(c.subscriptions, sub)

	if c.conn != nil && (c.apiKey == "" || c.isAuthenticated) {
		c.sendSubscribeMessage(sub)
	}
}

func (c *BitgetWSClient) sendSubscribeMessage(sub map[string]string) {
	msg := map[string]interface{}{
		"op":   "subscribe",
		"args": []map[string]string{sub},
	}
	c.sendMessage(msg)
}

func (c *BitgetWSClient) subscribeToChannels() {
	for _, sub := range c.subscriptions {
		c.sendSubscribeMessage(sub)
	}
}

func (c *BitgetWSClient) handleMessages() {
	defer close(c.done)
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Println("读取消息错误:", err)
			c.scheduleReconnect()
			return
		}

		// 处理pong响应
		if string(message) == "pong" {
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Println("解析JSON错误:", err)
			continue
		}

		switch {
		case msg["event"] == "login":
			if code, ok := msg["code"].(float64); ok && code == 0 {
				c.isAuthenticated = true
				c.subscribeToChannels()
			}
		case msg["event"] == "subscribe":
			log.Printf("订阅成功: %v", msg["args"])
		case msg["action"] == "snapshot" || msg["action"] == "update":
			log.Printf("收到市场数据: %v", msg["data"])
		}
	}
}

func (c *BitgetWSClient) scheduleReconnect() {
	time.AfterFunc(c.reconnectInterval, func() {
		log.Println("尝试重新连接...")
		if err := c.Connect(); err != nil {
			log.Println("重连失败:", err)
		}
	})
}

func (c *BitgetWSClient) sendMessage(msg interface{}) {
	if c.conn == nil {
		return
	}

	switch v := msg.(type) {
	case string:
		c.conn.WriteMessage(websocket.TextMessage, []byte(v))
	default:
		data, _ := json.Marshal(msg)
		c.conn.WriteMessage(websocket.TextMessage, data)
	}
}

func (c *BitgetWSClient) Disconnect() {
	close(c.done)
	if c.pingTicker != nil {
		c.pingTicker.Stop()
	}
	if c.conn != nil {
		c.conn.Close()
	}
}

// 使用示例
func main() {
	client := NewBitgetWSClient(
		"", // API Key
		"", // Secret Key
		"", // Passphrase
		"http://127.0.0.1:7890",
		5*time.Second,
	)

	client.Subscribe("SPOT", "ticker", "ETHUSDT")

	if err := client.Connect(); err != nil {
		log.Fatal("连接失败:", err)
	}

	time.Sleep(10 * time.Minute)
	client.Disconnect()
}
