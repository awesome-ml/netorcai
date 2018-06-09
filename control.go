package netorcai

import (
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"net"
	"sync"
	"time"
)

// Game state
const (
	GAME_NOT_RUNNING = iota
	GAME_RUNNING     = iota
	GAME_FINISHED    = iota
)

// Client state
const (
	CLIENT_UNLOGGED = iota
	CLIENT_LOGGED   = iota
	CLIENT_READY    = iota
	CLIENT_THINKING = iota
	CLIENT_KICKED   = iota
)

type GlobalState struct {
	Mutex sync.Mutex

	Listener net.Listener

	GameState int

	GameLogic []*GameLogicClient
	Players   []*PlayerOrVisuClient
	Visus     []*PlayerOrVisuClient

	NbPlayersMax                int
	NbVisusMax                  int
	NbTurnsMax                  int
	MillisecondsBeforeFirstTurn float64
	MillisecondsBetweenTurns    float64
}

func handleClient(client *Client, globalState *GlobalState,
	gameLogicExit chan int) {
	log.WithFields(log.Fields{
		"remote address": client.Conn.RemoteAddr(),
	}).Debug("New connection")

	defer client.Conn.Close()

	go readClientMessages(client)

	msg := <-client.incomingMessages
	if msg.err != nil {
		log.WithFields(log.Fields{
			"err":            msg.err,
			"remote address": client.Conn.RemoteAddr(),
		}).Debug("Cannot receive client first message")
		Kick(client, fmt.Sprintf("Invalid first message: %v", msg.err.Error()))
		return
	}

	loginMessage, err := readLoginMessage(msg.content)
	if err != nil {
		log.WithFields(log.Fields{
			"err":            err,
			"remote address": client.Conn.RemoteAddr(),
		}).Debug("Cannot read LOGIN message")
		Kick(client, fmt.Sprintf("Invalid first message: %v", err.Error()))
		return
	}
	client.nickname = loginMessage.nickname

	globalState.Mutex.Lock()
	switch loginMessage.role {
	case "player":
		if globalState.GameState != GAME_NOT_RUNNING {
			globalState.Mutex.Unlock()
			Kick(client, "LOGIN denied: Game has been started")
		} else if len(globalState.Players) >= globalState.NbPlayersMax {
			globalState.Mutex.Unlock()
			Kick(client, "LOGIN denied: Maximum number of players reached")
		} else {
			err = sendLoginACK(client)
			if err != nil {
				globalState.Mutex.Unlock()
				Kick(client, "LOGIN denied: Could not send LOGIN_ACK")
			} else {
				pvClient := &PlayerOrVisuClient{
					client:     client,
					playerID:   -1,
					isPlayer:   true,
					gameStarts: make(chan MessageGameStarts),
					newTurn:    make(chan MessageTurn),
					gameEnds:   make(chan MessageGameEnds),
				}

				globalState.Players = append(globalState.Players, pvClient)

				log.WithFields(log.Fields{
					"nickname":       client.nickname,
					"remote address": client.Conn.RemoteAddr(),
					"player count":   len(globalState.Players),
				}).Info("New player accepted")

				globalState.Mutex.Unlock()

				// Player behavior is handled in dedicated function.
				handlePlayerOrVisu(pvClient, globalState)
			}
		}
	case "visualization":
		if len(globalState.Visus) >= globalState.NbVisusMax {
			globalState.Mutex.Unlock()
			Kick(client, "LOGIN denied: Maximum number of visus reached")
		} else {
			err = sendLoginACK(client)
			if err != nil {
				globalState.Mutex.Unlock()
				Kick(client, "LOGIN denied: Could not send LOGIN_ACK")
			} else {
				pvClient := &PlayerOrVisuClient{
					client:     client,
					playerID:   -1,
					isPlayer:   false,
					gameStarts: make(chan MessageGameStarts),
					newTurn:    make(chan MessageTurn),
					gameEnds:   make(chan MessageGameEnds),
				}

				globalState.Visus = append(globalState.Visus, pvClient)

				log.WithFields(log.Fields{
					"nickname":       client.nickname,
					"remote address": client.Conn.RemoteAddr(),
					"visu count":     len(globalState.Visus),
				}).Info("New visualization accepted")

				globalState.Mutex.Unlock()

				// Visu behavior is handled in dedicated function.
				handlePlayerOrVisu(pvClient, globalState)
			}
		}
	case "game logic":
		if globalState.GameState != GAME_NOT_RUNNING {
			globalState.Mutex.Unlock()
			Kick(client, "LOGIN denied: Game has been started")
		} else if len(globalState.GameLogic) >= 1 {
			globalState.Mutex.Unlock()
			Kick(client, "LOGIN denied: A game logic is already logged in")
		} else {
			err = sendLoginACK(client)
			if err != nil {
				globalState.Mutex.Unlock()
				Kick(client, "LOGIN denied: Could not send LOGIN_ACK")
			} else {
				glClient := &GameLogicClient{
					client:       client,
					playerAction: make(chan MessageDoTurnPlayerAction),
					start:        make(chan int),
				}

				globalState.GameLogic = append(globalState.GameLogic, glClient)

				log.WithFields(log.Fields{
					"nickname":       client.nickname,
					"remote address": client.Conn.RemoteAddr(),
				}).Info("Game logic accepted")

				globalState.Mutex.Unlock()

				handleGameLogic(glClient, globalState, gameLogicExit)
			}
		}
	}
}

func Kick(client *Client, reason string) {
	if client.state == CLIENT_KICKED {
		return
	}

	client.state = CLIENT_KICKED
	log.WithFields(log.Fields{
		"remote address": client.Conn.RemoteAddr(),
		"nickname":       client.nickname,
		"reason":         reason,
	}).Warn("Kicking client")

	msg := MessageKick{
		MessageType: "KICK",
		KickReason:  reason,
	}

	content, err := json.Marshal(msg)
	if err == nil {
		_ = sendMessage(client, content)
		time.Sleep(time.Duration(500) * time.Millisecond)
	}
}

func sendLoginACK(client *Client) error {
	msg := MessageLoginAck{
		MessageType: "LOGIN_ACK",
	}

	content, err := json.Marshal(msg)
	if err == nil {
		err = sendMessage(client, content)
	}
	return err
}

func Cleanup() {
	globalGS.Mutex.Lock()
	log.Warn("Closing listening socket.")
	globalGS.Listener.Close()

	nbClients := len(globalGS.Players) + len(globalGS.Visus) +
		len(globalGS.GameLogic)
	if nbClients > 0 {
		log.Warn("Sending KICK messages to clients")
		kickChan := make(chan int)
		for _, client := range append(globalGS.Players, globalGS.Visus...) {
			go func(c *Client) {
				Kick(c, "netorcai abort")
				kickChan <- 0
			}(client.client)
		}
		for _, client := range globalGS.GameLogic {
			go func(c *Client) {
				Kick(c, "netorcai abort")
				kickChan <- 0
			}(client.client)
		}

		for i := 0; i < nbClients; i++ {
			<-kickChan
		}

		log.Warn("Closing client sockets")
		for _, client := range append(globalGS.Players, globalGS.Visus...) {
			client.client.Conn.Close()
		}
		for _, client := range globalGS.GameLogic {
			client.client.Conn.Close()
		}
	}

	globalGS.Mutex.Unlock()
}
