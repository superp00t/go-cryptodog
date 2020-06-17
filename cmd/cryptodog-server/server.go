package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"cryptodog-server/proto"
	"github.com/gorilla/websocket"
)

type User struct {
	Name  string
	Sendc chan proto.SpecificMessage

	// Channel that Sendc listener routine closes before exiting.
	// This tells any routines trying to send on Sendc that the user has left and they should be removed.
	Leavec chan interface{}
}

type Room struct {
	Name  string
	Users map[string]*User

	// Must be acquired before reading or writing to any fields, including individual users.
	Mutex sync.Mutex
}

var allRooms = make(map[string]*Room)
var allRoomsMutex = sync.Mutex{}
var upgrader = websocket.Upgrader{}

// Read a single Message from a client. Manual unpacking into a SpecificMessage is necessary.
func readMessage(c *websocket.Conn) (*proto.Message, error) {
	_, b, err := c.ReadMessage()
	if err != nil {
		return nil, err
	}

	var msg proto.Message
	return &msg, json.Unmarshal(b, &msg)
}

// Send a SpecificMessage to a single client and return success boolean. May block!
func sendMessage(user *User, msg proto.SpecificMessage) bool {
	select {
	case user.Sendc <- msg:
		return true
	case <-user.Leavec:
		// User is not listening on their Sendc channel anymore.
		return false
	}
}

// Send a SpecificMessage to a group of clients. May block!
func broadcastMessage(users map[string]*User, msg proto.SpecificMessage) {
	for _, user := range users {
		// XXX: ignoring return value
		sendMessage(user, msg)
	}
}

func ws(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer c.Close()

	errc := make(chan error, 1)
	sendc := make(chan proto.SpecificMessage)
	leavec := make(chan interface{})

	var room *Room
	user := User{
		Sendc:  sendc,
		Leavec: leavec,
	}
	hasJoined := false

	// Receive, unpack, and handle messages from client.
	go func() {
		for {
			msg, err := readMessage(c)
			if err != nil {
				errc <- err
				return
			}

			switch msg.Type {
			case proto.TypeJoinMessage:
				if hasJoined {
					err = fmt.Errorf("You have already joined a room.")
					break
				}

				decoded := proto.JoinMessage{}
				err = json.Unmarshal(msg.Raw, &decoded)
				if err != nil {
					errc <- err
					return
				}
				room, err = handleJoinMessage(&decoded, &user)
				if err == nil {
					hasJoined = true
				}

			case proto.TypeLeaveMessage:
				if !hasJoined {
					err = fmt.Errorf("You need to join a room to do that.")
					break
				}

				handleLeaveMessage(room, &user)
				hasJoined = false
				room = nil
				user.Name = ""

			case proto.TypeGroupMessage:
				if !hasJoined {
					err = fmt.Errorf("You need to join a room to do that.")
					break
				}

				decoded := proto.GroupMessage{}
				err = json.Unmarshal(msg.Raw, &decoded)
				if err != nil {
					errc <- err
					return
				}
				err = handleGroupMessage(&decoded, room, &user)

			case proto.TypePrivateMessage:
				if !hasJoined {
					err = fmt.Errorf("You need to join a room to do that.")
					break
				}

				decoded := proto.PrivateMessage{}
				err = json.Unmarshal(msg.Raw, &decoded)
				if err != nil {
					errc <- err
					return
				}
				err = handlePrivateMessage(&decoded, room, &user)

			default:
				errc <- fmt.Errorf("unknown message type: %v", msg.Type)
				return
			}

			if err != nil {
				// We can handle this type of error by passing it to the client.
				if !sendMessage(&user, &proto.ErrorMessage{
					Error: err.Error(),
				}) {
					errc <- fmt.Errorf("failed to send error to client (%v)", err.Error())
					return
				}
			}
		}
	}()

	// Listen for packed messages to send to the client.
	done := make(chan bool, 1)
	go func() {
		for {
			select {
			case msg := <-sendc:
				/* XXX: potentially blocking operation.
				   Can't just start a new thread w/ mutex; that might lead to messages not being scheduled for delivery in the order they were intended.
				   A workaround might be to give Sendc a buffer, but of what size? */
				err = c.WriteJSON(msg.Pack())
				if err != nil {
					errc <- err
					return
				}
			case <-done:
				close(leavec)
				return
			}
		}
	}()

	log.Println(<-errc)
	// Tell Sendc thread to clean up.
	done <- true
	if hasJoined {
		handleLeaveMessage(room, &user)
	}
}

func handleJoinMessage(msg *proto.JoinMessage, user *User) (*Room, error) {
	if len(msg.Room) == 0 || len(msg.Room) > 128 {
		return nil, fmt.Errorf("Room name must be between 1 and 128 characters.")
	}
	if len(msg.Name) == 0 || len(msg.Name) > 128 {
		return nil, fmt.Errorf("Nickname must be between 1 and 128 characters.")
	}

	allRoomsMutex.Lock()
	defer allRoomsMutex.Unlock()

	var room *Room
	if r, ok := allRooms[msg.Room]; ok {
		// XXX: we keep lock on all rooms to prevent the room from getting destroyed here
		// TODO: measure performance impact of joins/leaves
		r.Mutex.Lock()
		if _, ok := r.Users[msg.Name]; ok {
			r.Mutex.Unlock()
			return nil, fmt.Errorf("Nickname in use.")
		}

		r.Users[msg.Name] = user
		/* It's safe to unlock the room here because:
		   A) We're done updating r.Users, so concurrent reads won't cause a problem
		   B) Nothing else can update r.Users without acquiring allRoomsMutex first, which we're still holding */
		r.Mutex.Unlock()
		room = r
	} else {
		// Create new room.
		room = &Room{
			Name:  msg.Room,
			Users: make(map[string]*User),
		}
		room.Users[msg.Name] = user
		allRooms[msg.Room] = room
	}
	user.Name = msg.Name

	// Collect room roster and send to current user.
	var curUsers []string
	for name := range room.Users {
		if name != user.Name {
			curUsers = append(curUsers, name)
		}
	}
	sendMessage(user, &proto.RosterMessage{
		Users: curUsers,
	})

	// Alert room to new user.
	broadcastMessage(room.Users, &proto.JoinMessage{
		Name: msg.Name,
	})

	return room, nil
}

func handleLeaveMessage(room *Room, user *User) {
	// Global locking order to avoid deadlock: all rooms first, then specific room.
	allRoomsMutex.Lock()
	room.Mutex.Lock()
	delete(room.Users, user.Name)

	// Inform room that we left.
	broadcastMessage(room.Users, &proto.LeaveMessage{
		Name: user.Name,
	})

	// We were the last user in the room; now destroy it.
	if len(room.Users) == 0 {
		delete(allRooms, room.Name)
	}

	room.Mutex.Unlock()
	allRoomsMutex.Unlock()
}

func handleGroupMessage(msg *proto.GroupMessage, room *Room, user *User) error {
	room.Mutex.Lock()
	broadcastMessage(room.Users, &proto.GroupMessage{
		From: user.Name,
		Text: msg.Text,
	})
	room.Mutex.Unlock()
	return nil
}

func handlePrivateMessage(msg *proto.PrivateMessage, room *Room, user *User) error {
	room.Mutex.Lock()
	defer room.Mutex.Unlock()

	if to, ok := room.Users[msg.To]; ok {
		if !sendMessage(to, &proto.PrivateMessage{
			From: user.Name,
			Text: msg.Text,
		}) {
			return fmt.Errorf("Failed to deliver private message to recipient.")
		}
		return nil
	}
	return fmt.Errorf("Recipient not in room.")
}

func main() {
	http.HandleFunc("/ws", ws)
	log.Fatal(http.ListenAndServe("localhost:8009", nil))
}
