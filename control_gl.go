package netorcai

import (
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"math/rand"
	"sort"
	"time"
)

type GameLogicClient struct {
	client *Client
	// Messages to aggregate from player clients
	playerAction chan MessageDoTurnPlayerAction
	// Control messages
	start              chan int
	playerDisconnected chan int
}

func waitGameLogicFinition(glClient *GameLogicClient) {
	// As the GL coroutine is central, it does not finish directly.
	// It waits for the main coroutine to be OK with it first.
	// (making sure that all other clients have been kicked first).
	for {
		select {
		case <-glClient.client.canTerminate:
			return
		case <-glClient.playerAction:
		case <-glClient.playerDisconnected:
		case <-glClient.client.incomingMessages:
		}
	}
}

func handleGameLogic(glClient *GameLogicClient, globalState *GlobalState,
	onexit chan int) {
	// Wait for the game to start
	select {
	case <-glClient.start:
		log.Info("Starting game")
	case <-glClient.client.canTerminate:
		return
	case msg := <-glClient.client.incomingMessages:
		LockGlobalStateMutex(globalState, "GL first message", "GL")
		if msg.err == nil {
			Kick(glClient.client, "Received a game logic message but "+
				"the game has not started")
		} else {
			Kick(glClient.client, fmt.Sprintf("Game logic error. %v",
				msg.err.Error()))
		}
		globalState.GameLogic = globalState.GameLogic[:0]
		UnlockGlobalStateMutex(globalState, "GL first message", "GL")
		onexit <- 1
		waitGameLogicFinition(glClient)
		return
	}

	LockGlobalStateMutex(globalState, "Init + send DO_INIT", "GL")
	// Generate randomized player identifiers
	initialNbPlayers := len(globalState.Players)
	playerIDs := rand.Perm(len(globalState.Players))
	for playerIndex, player := range globalState.Players {
		player.playerID = playerIDs[playerIndex]
	}

	// Generate player information
	playersInfo := []*PlayerInformation{}
	for _, player := range globalState.Players {
		info := &PlayerInformation{
			PlayerID:      player.playerID,
			Nickname:      player.client.nickname,
			RemoteAddress: player.client.Conn.RemoteAddr().String(),
			IsConnected:   true,
		}
		player.playerInfo = info
		playersInfo = append(playersInfo, info)
	}

	// Sort player information by player_id
	sort.Slice(playersInfo, func(i, j int) bool {
		return playersInfo[i].PlayerID < playersInfo[j].PlayerID
	})

	// Send DO_INIT
	err := sendDoInit(glClient, len(globalState.Players),
		globalState.NbTurnsMax)
	UnlockGlobalStateMutex(globalState, "Init + send DO_INIT", "GL")

	if err != nil {
		Kick(glClient.client, fmt.Sprintf("Cannot send DO_INIT. %v",
			err.Error()))
		onexit <- 1
		waitGameLogicFinition(glClient)
		return
	}

	// Wait for first turn (DO_INIT_ACK)
	var msg ClientMessage
	select {
	case <-glClient.client.canTerminate:
		return
	case msg = <-glClient.client.incomingMessages:
		if msg.err != nil {
			Kick(glClient.client,
				fmt.Sprintf("Cannot read DO_INIT_ACK. %v", msg.err.Error()))
			onexit <- 1
			waitGameLogicFinition(glClient)
			return
		}
	case <-time.After(3 * time.Second):
		Kick(glClient.client, "Did not receive DO_INIT_ACK after 3 seconds.")
		onexit <- 1
		waitGameLogicFinition(glClient)
		return
	}

	doTurnAckMsg, err := readDoInitAckMessage(msg.content)
	if err != nil {
		Kick(glClient.client,
			fmt.Sprintf("Invalid DO_INIT_ACK message. %v", err.Error()))
		onexit <- 1
		waitGameLogicFinition(glClient)
		return
	}

	// Send GAME_STARTS to all clients
	LockGlobalStateMutex(globalState, "Send GAME_STARTS", "GL")
	for _, player := range globalState.Players {
		player.gameStarts <- MessageGameStarts{
			MessageType:      "GAME_STARTS",
			PlayerID:         player.playerID,
			PlayersInfo:      []*PlayerInformation{},
			NbPlayers:        initialNbPlayers,
			NbTurnsMax:       globalState.NbTurnsMax,
			DelayFirstTurn:   globalState.MillisecondsBeforeFirstTurn,
			DelayTurns:       globalState.MillisecondsBetweenTurns,
			InitialGameState: doTurnAckMsg.InitialGameState,
		}
	}

	for _, visu := range globalState.Visus {
		visu.gameStarts <- MessageGameStarts{
			MessageType:      "GAME_STARTS",
			PlayerID:         visu.playerID,
			PlayersInfo:      playersInfo,
			NbPlayers:        initialNbPlayers,
			NbTurnsMax:       globalState.NbTurnsMax,
			DelayFirstTurn:   globalState.MillisecondsBeforeFirstTurn,
			DelayTurns:       globalState.MillisecondsBetweenTurns,
			InitialGameState: doTurnAckMsg.InitialGameState,
		}
	}
	UnlockGlobalStateMutex(globalState, "Send GAME_STARTS", "GL")

	if !globalState.Fast {
		// Wait before really starting the game
		log.WithFields(log.Fields{
			"duration (ms)": globalState.MillisecondsBeforeFirstTurn,
		}).Debug("Sleeping before first turn")
		time.Sleep(time.Duration(globalState.MillisecondsBeforeFirstTurn) *
			time.Millisecond)
	}

	// Order the game logic to compute a TURN (without any action)
	turnNumber := 0
	playerActions := make([]MessageDoTurnPlayerAction, 0)
	sendDoTurnToGL := make(chan int, 1)
	sendDoTurn(glClient, playerActions)

	for {
		select {
		case <-glClient.client.canTerminate:
			return
		case <-sendDoTurnToGL:
			// Send current actions
			sendDoTurn(glClient, playerActions)
			// Clear actions array
			playerActions = playerActions[:0]
		case action := <-glClient.playerAction:
			// A client sent its actions.
			// Replace the current message from this player if it exists,
			// and place it at the end of the array.
			// This may happen if the client was late in a previous turn but
			// catched up in current turn by sending two TURN_ACK.
			actionFound := false
			for actionIndex, act := range playerActions {
				if act.PlayerID == action.PlayerID {
					playerActions[len(playerActions)-1], playerActions[actionIndex] = playerActions[actionIndex], playerActions[len(playerActions)-1]
					playerActions[len(playerActions)-1] = action
					actionFound = true
					break
				}
			}

			if !actionFound {
				// Append the action into the actions array
				playerActions = append(playerActions, action)
			}

			if globalState.Fast {
				LockGlobalStateMutex(globalState, "Check player count", "GL")
				// Trigger a new TURN if all players have played
				if len(playerActions) == len(globalState.Players) {
					sendDoTurnToGL <- 1
				}
				UnlockGlobalStateMutex(globalState, "Check player count", "GL")
			}
		case <-glClient.playerDisconnected:
			if globalState.Fast {
				LockGlobalStateMutex(globalState, "Check player count", "GL")
				// Trigger a new TURN if all players have played
				if len(playerActions) == len(globalState.Players) {
					sendDoTurnToGL <- 1
				}
				UnlockGlobalStateMutex(globalState, "Check player count", "GL")
			}
		case msg := <-glClient.client.incomingMessages:
			// New message received from the game logic
			if msg.err != nil {
				Kick(glClient.client,
					fmt.Sprintf("Cannot read DO_TURN_ACK. %v",
						msg.err.Error()))
				onexit <- 1
				waitGameLogicFinition(glClient)
				return
			}

			doTurnAckMsg, err := readDoTurnAckMessage(msg.content,
				initialNbPlayers)
			if err != nil {
				Kick(glClient.client,
					fmt.Sprintf("Invalid DO_TURN_ACK message. %v",
						err.Error()))
				onexit <- 1
				waitGameLogicFinition(glClient)
				return
			}

			turnNumber = turnNumber + 1
			if turnNumber < globalState.NbTurnsMax {
				// Forward the TURN to the clients
				LockGlobalStateMutex(globalState, "Send TURN", "GL")
				for _, player := range globalState.Players {
					player.newTurn <- MessageTurn{
						MessageType: "TURN",
						TurnNumber:  turnNumber - 1,
						GameState:   doTurnAckMsg.GameState,
						PlayersInfo: []*PlayerInformation{},
					}
				}
				for _, visu := range globalState.Visus {
					visu.newTurn <- MessageTurn{
						MessageType: "TURN",
						TurnNumber:  turnNumber - 1,
						GameState:   doTurnAckMsg.GameState,
						PlayersInfo: playersInfo,
					}
				}

				// Trigger a new TURN if there is no player anymore
				if globalState.Fast && len(playerActions) == 0 {
					sendDoTurnToGL <- 1
				}

				UnlockGlobalStateMutex(globalState, "Send TURN", "GL")

				if !globalState.Fast {
					// Trigger a new DO_TURN in some time
					go func() {
						log.WithFields(log.Fields{
							"duration (ms)": globalState.MillisecondsBetweenTurns,
						}).Debug("Sleeping before next turn")
						time.Sleep(time.Duration(
							globalState.MillisecondsBetweenTurns) *
							time.Millisecond)

						sendDoTurnToGL <- 1
					}()
				}
			} else {
				if doTurnAckMsg.WinnerPlayerID != -1 {
					log.WithFields(log.Fields{
						"winner player ID":      doTurnAckMsg.WinnerPlayerID,
						"winner nickname":       playersInfo[doTurnAckMsg.WinnerPlayerID].Nickname,
						"winner remote address": playersInfo[doTurnAckMsg.WinnerPlayerID].RemoteAddress,
					}).Info("Game is finished")
				} else {
					log.Info("Game is finished (no winner!)")
				}

				// Send GAME_ENDS to all clients
				LockGlobalStateMutex(globalState, "Send GAME_ENDS", "GL")
				for _, player := range globalState.Players {
					player.gameEnds <- MessageGameEnds{
						MessageType:    "GAME_ENDS",
						WinnerPlayerID: doTurnAckMsg.WinnerPlayerID,
						GameState:      doTurnAckMsg.GameState,
					}
				}
				for _, visu := range globalState.Visus {
					visu.gameEnds <- MessageGameEnds{
						MessageType:    "GAME_ENDS",
						WinnerPlayerID: doTurnAckMsg.WinnerPlayerID,
						GameState:      doTurnAckMsg.GameState,
					}
				}
				UnlockGlobalStateMutex(globalState, "Send GAME_ENDS", "GL")

				// Leave the program
				Kick(glClient.client, "Game is finished")
				onexit <- 0
				waitGameLogicFinition(glClient)
				return
			}
		}
	}
}

func sendDoInit(client *GameLogicClient, nbPlayers, nbTurnsMax int) error {
	msg := MessageDoInit{
		MessageType: "DO_INIT",
		NbPlayers:   nbPlayers,
		NbTurnsMax:  nbTurnsMax,
	}

	content, err := json.Marshal(msg)
	if err == nil {
		log.WithFields(log.Fields{
			"nickname":       client.client.nickname,
			"remote address": client.client.Conn.RemoteAddr(),
			"content":        string(content),
		}).Debug("Sending DO_INIT to game logic")
		err = sendMessage(client.client, content)
	}
	return err
}

func sendDoTurn(client *GameLogicClient,
	playerActions []MessageDoTurnPlayerAction) error {
	msg := MessageDoTurn{
		MessageType:   "DO_TURN",
		PlayerActions: playerActions,
	}

	content, err := json.Marshal(msg)
	if err == nil {
		log.WithFields(log.Fields{
			"nickname":       client.client.nickname,
			"remote address": client.client.Conn.RemoteAddr(),
			"content":        string(content),
		}).Debug("Sending DO_TURN to game logic")
		err = sendMessage(client.client, content)
	}
	return err
}
