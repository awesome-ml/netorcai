package test

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"regexp"
	"testing"
)

type ClientTurnAckFunc func(int) string

func helloGameLogic(t *testing.T, glClient *Client,
	nbPlayers, nbTurns int) {
	// Wait DO_INIT
	msg, err := waitReadMessage(glClient, 1000)
	assert.NoError(t, err, "Could not read GLClient message (DO_INIT)")
	checkDoInit(t, msg, nbPlayers, nbTurns)

	// Send DO_INIT_ACK
	data := `{"message_type":"DO_INIT_ACK", "initial_game_state":{"all_clients":{}}}`
	err = glClient.SendString(data)
	assert.NoError(t, err, "GLClient could not send DO_INIT_ACK")

	// Wait for DO_TURN
	for turn := 0; turn < nbTurns; turn++ {
		msg, err := waitReadMessage(glClient, 1000)
		assert.NoError(t, err, "Could not read GLClient message (DO_TURN) "+
			"%v/%v", turn, nbTurns)
		checkDoTurn(t, msg, nbPlayers, turn-1)

		// Send DO_TURN_ACK
		data = `{"message_type":"DO_TURN_ACK",
			"winner_player_id":-1,
			"game_state":{"all_clients":{}}}`
		err = glClient.SendString(data)
		assert.NoError(t, err, "GLClient could not send DO_TURN_ACK")
	}

	msg, err = waitReadMessage(glClient, 1000)
	assert.NoError(t, err, "Could not read GLClient message (KICK)")
	checkKick(t, msg, regexp.MustCompile(".*"))

	// Close socket
	glClient.Disconnect()
}

func DefaultHelloClientTurnAckGenerator(turn int) string {
	return fmt.Sprintf(`{"message_type": "TURN_ACK",
		"turn_number": %v,
		"actions": []}`, turn)
}

func helloClient(t *testing.T, client *Client, nbPlayers, nbTurnsGL,
	nbTurnsClient int, msBeforeFirstTurn, msBetweenTurns float64,
	isPlayer, shouldBeValid bool, turnAckFunc ClientTurnAckFunc) {
	// Wait GAME_STARTS
	msg, err := waitReadMessage(client, 1000)
	assert.NoError(t, err, "Could not read client message (GAME_STARTS)")
	checkGameStarts(t, msg, nbPlayers, nbTurnsGL, msBeforeFirstTurn,
		msBetweenTurns, isPlayer)

	for turn := 0; turn < nbTurnsClient-1; turn++ {
		// Wait TURN
		msg, err := waitReadMessage(client, 1000)
		assert.NoError(t, err, "Could not read client message (TURN) "+
			"%v/%v", turn, nbTurnsClient)
		checkTurn(t, msg, nbPlayers, turn, isPlayer)

		// Send TURN_ACK
		data := turnAckFunc(turn)
		err = client.SendString(data)
		assert.NoError(t, err, "Client cannot send TURN_ACK")
	}

	if shouldBeValid {
		// Wait GAME_ENDS
		msg, err = waitReadMessage(client, 1000)
		assert.NoError(t, err, "Could not read client message (GAME_ENDS)")
		checkGameEnds(t, msg)

		// Wait Kick
		msg, err = waitReadMessage(client, 1000)
		assert.NoError(t, err, "Could not read client message (KICK)")
		checkKick(t, msg, regexp.MustCompile(`Game is finished`))
	} else {
		// Wait Kick
		msg, err = waitReadMessage(client, 1000)
		assert.NoError(t, err, "Could not read client message (KICK)")
		checkKick(t, msg, regexp.MustCompile(`.*`))
	}

	// Close socket
	client.Disconnect()
}
