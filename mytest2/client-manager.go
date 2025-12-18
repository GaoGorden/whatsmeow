package mytest2

import (
	"sync"

	"go.mau.fi/whatsmeow"
)

// ClientManager 维护所有的 whatsmeow 客户端实例
type ClientManager struct {
	// Clients: key 是用户的 JID (string)，value 是该用户的 whatsmeow.Client
	Clients map[string]*whatsmeow.Client

	Mu sync.RWMutex
}

var (
	globalManager *ClientManager
	once          sync.Once
)

// GetManager 提供一个获取实例的方法（可选，但更规范）
func GetManager() *ClientManager {
	return globalManager
}

func InitManager() {
	// 即使 1000 个请求同时进来，NewSessionManager 也只会执行 1 次
	once.Do(func() {
		globalManager = &ClientManager{
			Clients: make(map[string]*whatsmeow.Client),
		}
	})
}

// RegisterClient 提供注册方法
func (sm *ClientManager) RegisterClient(userID string, client *whatsmeow.Client) {
	sm.Mu.Lock()
	defer sm.Mu.Unlock()
	sm.Clients[userID] = client
}

// GetClient 提供获取方法
func (sm *ClientManager) GetClient(userID string) (*whatsmeow.Client, bool) {
	sm.Mu.RLock()
	defer sm.Mu.RUnlock()
	cli, ok := sm.Clients[userID]
	return cli, ok
}
