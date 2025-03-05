package inhibitor

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
)

type InhibitorType string

const (
	TypeBlock InhibitorType = "block"
	TypeDelay InhibitorType = "delay"
)

type Inhibitor struct {
	Who  string
	What string
	Why  string
	Type InhibitorType
	Conn net.Conn
}

type Manager struct {
	logger     *log.Logger
	socketPath string
	listener   net.Listener
	mutex      sync.RWMutex
	inhibitors map[net.Conn]*Inhibitor
	onChange   func()
}

func NewManager(logger *log.Logger, socketPath string, onChange func()) (*Manager, error) {
	if _, err := os.Stat(socketPath); err == nil {
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("failed to remove existing socket: %v", err)
		}
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create socket: %v", err)
	}

	manager := &Manager{
		logger:     logger,
		socketPath: socketPath,
		listener:   listener,
		inhibitors: make(map[net.Conn]*Inhibitor),
		onChange:   onChange,
	}

	go manager.acceptConnections()

	return manager, nil
}

func (m *Manager) acceptConnections() {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok {
				if syscallErr, ok := opErr.Err.(*os.SyscallError); ok {
					if syscallErr.Err == syscall.EINVAL {
						return
					}
				}
				if opErr.Err.Error() == "use of closed network connection" {
					return
				}
			}
			m.logger.Printf("Failed to accept connection: %v", err)
			continue
		}

		go m.handleConnection(conn)
	}
}

func (m *Manager) handleConnection(conn net.Conn) {
	if _, err := conn.Write([]byte{0}); err != nil {
		m.logger.Printf("Failed to send acknowledgment: %v", err)
		conn.Close()
		return
	}

	inhibitor := &Inhibitor{
		Who:  "connection-based",
		What: "connection-based",
		Why:  "connection-based",
		Type: TypeBlock,
		Conn: conn,
	}

	m.mutex.Lock()
	m.inhibitors[conn] = inhibitor
	m.mutex.Unlock()

	m.logger.Printf("New inhibitor connected: %s", conn.RemoteAddr())
	if m.onChange != nil {
		m.onChange()
	}

	buf := make([]byte, 1)
	for {
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
	}

	m.mutex.Lock()
	delete(m.inhibitors, conn)
	m.mutex.Unlock()

	m.logger.Printf("Inhibitor disconnected: %s", conn.RemoteAddr())
	if m.onChange != nil {
		m.onChange()
	}

	conn.Close()
}

func (m *Manager) AddInhibitor(who, what, why string, inhibitType InhibitorType) *Inhibitor {
	inhibitor := &Inhibitor{
		Who:  who,
		What: what,
		Why:  why,
		Type: inhibitType,
	}

	m.logger.Printf("Added inhibitor: %s (%s) by %s for %s", what, string(inhibitType), who, why)
	if m.onChange != nil {
		m.onChange()
	}

	return inhibitor
}

func (m *Manager) RemoveInhibitor(inhibitor *Inhibitor) {
	if inhibitor.Conn != nil {
		m.mutex.Lock()
		delete(m.inhibitors, inhibitor.Conn)
		m.mutex.Unlock()
	}

	m.logger.Printf("Removed inhibitor: %s (%s) by %s", inhibitor.What, string(inhibitor.Type), inhibitor.Who)
	if m.onChange != nil {
		m.onChange()
	}
}

func (m *Manager) GetInhibitors() []*Inhibitor {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	inhibitors := make([]*Inhibitor, 0, len(m.inhibitors))
	for _, inhibitor := range m.inhibitors {
		inhibitors = append(inhibitors, inhibitor)
	}

	return inhibitors
}

func (m *Manager) HasBlockingInhibitors() bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	for _, inhibitor := range m.inhibitors {
		if inhibitor.Type == TypeBlock {
			return true
		}
	}

	return false
}

func (m *Manager) Close() error {
	if err := m.listener.Close(); err != nil {
		return fmt.Errorf("failed to close listener: %v", err)
	}

	m.mutex.Lock()
	for conn := range m.inhibitors {
		conn.Close()
	}
	m.inhibitors = make(map[net.Conn]*Inhibitor)
	m.mutex.Unlock()

	if err := os.Remove(m.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove socket file: %v", err)
	}

	return nil
}
