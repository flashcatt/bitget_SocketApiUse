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
	conn              *websocket.Conn     // WebSocket 连接
	apiKey            string              // API 密钥
	secretKey         string              // API 密钥的Secret
	passphrase        string              // API 密钥的Passphrase
	reconnectInterval time.Duration       // 重连间隔
	proxyUrl          string              // 代理 URL
	isAuthenticated   bool                // 是否已认证
	subscriptions     []map[string]string // 订阅列表
	pingTicker        *time.Ticker        // 心跳定时器
	done              chan struct{}       // 关闭通道
}

func NewBitgetWSClient(apiKey, secretKey, passphrase, proxyUrl string, reconnectInterval time.Duration) *BitgetWSClient {
	return &BitgetWSClient{
		reconnectInterval: reconnectInterval,            // 重连间隔
		apiKey:            apiKey,                       // API 密钥
		secretKey:         secretKey,                    // API 密钥的Secret
		passphrase:        passphrase,                   // API 密钥的Passphrase
		proxyUrl:          proxyUrl,                     // 代理 URL
		subscriptions:     make([]map[string]string, 0), // 订阅列表
		done:              make(chan struct{}),          // 关闭通道
	}
}

func (c *BitgetWSClient) Connect() error { // 连接 Bitget WebSocket 服务器
	// 设置代理
	proxy, _ := url.Parse(c.proxyUrl) // 解析代理 URL
	transport := &http.Transport{     // 创建 HTTP 传输
		Proxy: http.ProxyURL(proxy), // 设置代理
	}

	dialer := websocket.DefaultDialer // 创建 WebSocket 连接
	dialer.Proxy = transport.Proxy    // 设置代理

	conn, _, err := dialer.Dial("wss://ws.bitget.com/v2/ws/public", nil) // 连接 Bitget WebSocket 服务器
	if err != nil {
		return err
	}
	c.conn = conn // 连接 Bitget WebSocket 服务器

	// 启动心跳
	c.startPingPong() // 启动心跳

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

func (c *BitgetWSClient) startPingPong() { // 启动心跳
	c.pingTicker = time.NewTicker(30 * time.Second) // 30秒发送一次心跳
	go func() {                                     // 启动心跳 goroutine
		for { // 无限循环
			select {
			case <-c.pingTicker.C: // 30秒定时器触发
				c.sendMessage("ping") // 发送心跳消息

			case <-c.done: // 关闭通道触发
				return
			}
		}
	}()
}

func (c *BitgetWSClient) authenticate() { // 认证 Bitget WebSocket 服务器
	timestamp := time.Now().Unix()                         // 获取当前时间的 Unix 时间戳
	message := fmt.Sprintf("%dGET/user/verify", timestamp) // 构建待签名字符串

	h := hmac.New(sha256.New, []byte(c.secretKey))        // 创建 HMAC-SHA256 哈希对象
	h.Write([]byte(message))                              // 写入待签名字符串到哈希对象
	sign := base64.StdEncoding.EncodeToString(h.Sum(nil)) // 计算哈希值并编码为 Base64 字符串

	loginMsg := map[string]interface{}{ // 构建登录消息
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

func (c *BitgetWSClient) Subscribe(instType, channel, instId string) { // 订阅 Bitget WebSocket 服务器
	sub := map[string]string{ // 构建订阅消息
		"instType": instType, // 产品类型
		"channel":  channel,  // 通道
		"instId":   instId,   // 产品 ID
	}
	c.subscriptions = append(c.subscriptions, sub) // 订阅列表添加订阅消息

	if c.conn != nil && (c.apiKey == "" || c.isAuthenticated) { // 连接已建立且未认证
		c.sendSubscribeMessage(sub) // 发送订阅消息
	}
}

func (c *BitgetWSClient) sendSubscribeMessage(sub map[string]string) { // 发送订阅消息
	msg := map[string]interface{}{ // 构建订阅消息
		"op":   "subscribe",
		"args": []map[string]string{sub}, // 订阅参数
	}
	c.sendMessage(msg) // 发送订阅消息
}

func (c *BitgetWSClient) subscribeToChannels() { // 订阅所有通道
	for _, sub := range c.subscriptions { // 遍历订阅列表
		c.sendSubscribeMessage(sub) // 发送订阅消息
	}
}

func (c *BitgetWSClient) handleMessages() { // 处理消息
	defer close(c.done) // 关闭通道
	for {
		_, message, err := c.conn.ReadMessage() // 读取消息
		if err != nil {
			log.Println("读取消息错误:", err) // 打印读取消息错误
			c.scheduleReconnect()       // 调度重连
			return
		}

		// 处理pong响应
		if string(message) == "pong" { // 如果是 pong 响应
			continue // 继续循环
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil { // 解析 JSON 消息
			log.Println("解析JSON错误:", err) // 打印解析 JSON 错误
			continue                      // 继续循环
		}

		switch {
		case msg["event"] == "login": // 如果是登录事件
			if code, ok := msg["code"].(float64); ok && code == 0 { // 如果登录成功
				c.isAuthenticated = true // 设置认证状态为 true
				c.subscribeToChannels()  // 订阅所有通道
			}
		case msg["event"] == "subscribe": // 如果是订阅事件
			log.Printf("订阅成功: %v", msg["args"]) // 打印订阅成功消息
		case msg["action"] == "snapshot" || msg["action"] == "update": // 如果是快照或更新事件
			log.Printf("收到市场数据: %v", msg["data"]) // 打印收到的市场数据
		}
	}
}

func (c *BitgetWSClient) scheduleReconnect() { // 调度重连
	time.AfterFunc(c.reconnectInterval, func() { // 30秒定时器触发
		log.Println("尝试重新连接...")            // 打印尝试重新连接消息
		if err := c.Connect(); err != nil { // 连接 Bitget WebSocket 服务器
			log.Println("重连失败:", err) // 打印重连失败消息
		}
	})
}

func (c *BitgetWSClient) sendMessage(msg interface{}) { // 发送消息
	if c.conn == nil {
		return
	}

	switch v := msg.(type) {
	case string: // 如果是字符串类型
		c.conn.WriteMessage(websocket.TextMessage, []byte(v)) // 发送文本消息
	default: // 如果不是字符串类型
		data, _ := json.Marshal(msg)                     // 编码为 JSON 字符串
		c.conn.WriteMessage(websocket.TextMessage, data) // 发送文本消息
	}
}

func (c *BitgetWSClient) Disconnect() { // 断开连接
	close(c.done)            // 关闭通道
	if c.pingTicker != nil { // 如果心跳定时器存在
		c.pingTicker.Stop() // 停止心跳定时器
	}
	if c.conn != nil { // 如果连接存在
		c.conn.Close() // 关闭连接
	}
}

// 使用示例
func main() {
	client := NewBitgetWSClient( // 创建 Bitget WebSocket 客户端
		"", // API Key
		"", // Secret Key
		"", // Passphrase
		"http://127.0.0.1:7890",
		5*time.Second, // 重连间隔
	)

	client.Subscribe("SPOT", "ticker", "ETHUSDT") // 订阅 ETHUSDT 合约的 ticker 数据

	if err := client.Connect(); err != nil {
		log.Fatal("连接失败:", err)
	}

	time.Sleep(10 * time.Minute) // 运行 10 分钟
	client.Disconnect()          // 断开连接
}
