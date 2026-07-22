package mytest2

import (
	"context"
	"sync"

	"go.mau.fi/whatsmeow"
)

// ClientManager 维护所有的 whatsmeow 客户端实例
type ClientManager struct {
	// ClientEntries: key 是用户的 JID (string)，value 是该用户的 whatsmeow.Client
	ClientEntries map[string]*ClientEntry

	Mu sync.RWMutex
}

type ClientEntry struct {
	Client     *whatsmeow.Client
	CancelFunc context.CancelFunc // 用于停止该特定的协程
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
			ClientEntries: make(map[string]*ClientEntry),
		}
	})
}

// RegisterClient 提供注册方法
func (sm *ClientManager) RegisterClient(userID string, client *whatsmeow.Client, cancel context.CancelFunc) {
	sm.Mu.Lock()
	defer sm.Mu.Unlock()
	sm.ClientEntries[userID] = &ClientEntry{Client: client, CancelFunc: cancel}
}

// GetClient 提供获取方法
func (sm *ClientManager) GetClient(userID string) (*ClientEntry, bool) {
	sm.Mu.RLock()
	defer sm.Mu.RUnlock()
	cli, ok := sm.ClientEntries[userID]
	return cli, ok
}

// UnregisterAndStop 提供删除并停止协程的方法
func (sm *ClientManager) UnregisterAndStop(userId string) {
	sm.Mu.Lock()
	defer sm.Mu.Unlock()
	if entry, ok := sm.ClientEntries[userId]; ok {
		entry.CancelFunc() // 触发该特定协程的 ctx.Done()
		delete(sm.ClientEntries, userId)
	}
}
