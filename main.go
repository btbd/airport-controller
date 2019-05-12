package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	uuid "github.com/satori/go.uuid"
	"pack.ag/amqp"
)

var airport struct {
	Disabled  bool            `json:"disabled"`
	Suppliers []*Supplier     `json:"suppliers"`
	Retailers []*Retailer     `json:"retailers"`
	Carriers  []*Carrier      `json:"carriers"`
	Mutex     sync.RWMutex    `json:"-"`
	Sender    *amqp.Sender    `json:"-"`
	Context   context.Context `json:"-"`
}

var upgrader = websocket.Upgrader{}
var clients = []chan string{}
var clients_mu *sync.Mutex = &sync.Mutex{}
var ates = map[string]*ActiveTimeoutEvent{}

var Sizes = []string{"small", "medium", "large"}
var TimeoutEvents = []TimeoutEvent{
	{
		Type:    "Order.OrderStatus.OrderReleased",
		Source:  "Passenger",
		Timeout: 10 * time.Second,
		Data: map[string]interface{}{
			"orderStatus": "OrderReleased",
		},
		Expect: EXPECT_PROVIDER,
		Resend: false,
	},
	{
		Type:    "Order.OrderStatus.OrderReleased",
		Source:  "Retailer",
		Timeout: 10 * time.Second,
		Data: map[string]interface{}{
			"orderStatus": "OrderReleased",
		},
		Expect: EXPECT_SUPPLIER,
		Resend: true,
	},
	{
		Type:    "TransferAction.ActionStatus.PotentialActionStatus",
		Source:  "Supplier",
		Timeout: 10 * time.Second,
		Data: map[string]interface{}{
			"actionStatus": "PotentialActionStatus",
		},
		Expect: EXPECT_CARRIER,
		Resend: true,
	},
	{
		Type:    "TransferAction.ActionStatus.ArrivedActionStatus",
		Source:  "Controller",
		Timeout: 10 * time.Second,
		Data: map[string]interface{}{
			"actionStatus": "ArrivedActionStatus",
		},
		Expect: EXPECT_CARRIER,
		Resend: true,
	},
	{
		Type:    "TransferAction.ActionStatus.CompletedActionStatus",
		Source:  "Carrier",
		Timeout: 10 * time.Second,
		Data: map[string]interface{}{
			"actionStatus": "CompletedActionStatus",
		},
		Expect: EXPECT_RETAILER,
		Resend: false,
	},
}

type CloudEvent struct {
	SpecVersion string                     `json:"specversion,omitempty"`
	Type        string                     `json:"type,omitempty"`
	Source      string                     `json:"source,omitempty"`
	Subject     string                     `json:"subject,omitempty"`
	ID          string                     `json:"id,omitempty"`
	Time        string                     `json:"time,omitempty"`
	SchemaURL   string                     `json:"schemaurl,omitempty"`
	ContentType string                     `json:"contenttype,omitempty"`
	Extensions  map[string]json.RawMessage `json:"extensions,omitempty"`
	Data        json.RawMessage            `json:"data,omitempty"`
	Cause       string                     `json:"cause,omitempty"`
	DataObject  interface{}                `json:"-"`
}

const (
	EXPECT_PROVIDER = iota // Expect response from provider in data
	EXPECT_SUPPLIER = iota // Expect response from supplier that hanldes retailer (from source)
	EXPECT_RETAILER = iota // Expect response from retailer (from toLocation)
	EXPECT_CARRIER  = iota // Expect response from carrier that handles retailer and supplier (from toLocation and fromLocation)
)

type TimeoutEvent struct {
	Timeout time.Duration
	Type    string
	Source  string
	Data    map[string]interface{}
	Expect  int
	Resend  bool
}

type ActiveTimeoutEvent struct {
	Message *amqp.Message
	Timer   *time.Timer
}

const (
	CUSTOMER_WALKING   = iota
	CUSTOMER_INLINE    = iota
	CUSTOMER_ORDERING  = iota
	CUSTOMER_ORDERED   = iota
	CUSTOMER_SATISFIED = iota
)

const (
	SATISFY_OK    = iota
	SATISFY_FORCE = iota
	SATISFY_CLOSE = iota
)

type Customer struct {
	Retailer *Retailer   `json:"-"`
	State    int         `json:"state"`
	Id       string      `json:"-"`
	Client   chan string `json:"-"`
}

func (customer *Customer) Position() (int, int) {
	for ri, r := range airport.Retailers {
		for ci, c := range r.Customers {
			if c.Id == customer.Id {
				return ri, ci
			}
		}
	}

	return -1, -1
}

func (customer *Customer) Send(msg string) {
	go func() { customer.Client <- msg }()
}

func (customer *Customer) Order() {
	customer.State = CUSTOMER_ORDERING
	customer.Send("o")

	go func() {
		time.Sleep(time.Second * 10)

		airport.Mutex.Lock()
		if customer.State == CUSTOMER_ORDERING {
			customer.Satisfy(SATISFY_FORCE)
		}
		airport.Mutex.Unlock()
	}()
}

func (customer *Customer) Satisfy(kind int) {
	ri, ci := customer.Position()
	if ri == -1 || ci == -1 {
		return
	}

	customer.State = CUSTOMER_SATISFIED
	switch kind {
	case SATISFY_OK:
		customer.Send("s")
	case SATISFY_FORCE:
		customer.Send("f")
	case SATISFY_CLOSE:
		customer.Send("c")
	}

	Broadcast(`{"type":"satisfied","r":` + strconv.Itoa(ri) + `,"c":` + strconv.Itoa(ci) + `}`)

	var customers []*Customer
	if c := customer.Retailer.Customers; len(c) > 1 {
		customers = c[1:]
		if customers[0].State == CUSTOMER_INLINE {
			customers[0].Order()
		}
	}

	customer.Retailer.Customers = customers
}

func (customer *Customer) MarshalJSON() ([]byte, error) {
	return json.Marshal(*customer)
}

type SupplierJob struct {
	Retailer string   `json:"customer"`
	Offers   []string `json:"offer"`
}

func (sj *SupplierJob) MarshalJSON() ([]byte, error) {
	return json.Marshal(*sj)
}

type Supplier struct {
	Name string         `json:"name"`
	Logo string         `json:"logo"`
	Jobs []*SupplierJob `json:"jobs"`
}

func (supplier *Supplier) GetPosition() int {
	for si, s := range airport.Suppliers {
		if s.Name == supplier.Name {
			return si
		}
	}

	return -1
}

func (supplier *Supplier) Disconnect() {
	i := supplier.GetPosition()
	if i != -1 {
		airport.Suppliers = append(airport.Suppliers[:i], airport.Suppliers[i+1:]...)
		Broadcast(`{"type":"rmsupplier","s":` + strconv.Itoa(i) + `}`)
		UpdateJobs()
		PublishDisconnect(supplier.Name)
	}
}

func EventToMessage(event *CloudEvent) *amqp.Message {
	if event.SpecVersion == "" {
		event.SpecVersion = "0.3"
	}
	if event.ID == "" {
		event.ID = uuid.Must(uuid.NewV4()).String()
	}
	if event.Time == "" {
		event.Time = time.Now().Format(time.RFC3339Nano)
	}
	return &amqp.Message{
		Properties: &amqp.MessageProperties{
			ContentType: "application/json",
		},
		ApplicationProperties: map[string]interface{}{
			"cloudEvents:specversion": event.SpecVersion,
			"cloudEvents:type":        event.Type,
			"cloudEvents:source":      event.Source,
			"cloudEvents:subject":     event.Subject,
			"cloudEvents:id":          event.ID,
			"cloudEvents:time":        event.Time,
		},
		Data: [][]byte{event.Data},
	}
}

func (supplier *Supplier) UpdateJob() {
	body, _ := json.Marshal(supplier.Jobs)
	airport.Sender.Send(airport.Context, EventToMessage(&CloudEvent{
		Type:    "Offer.Product",
		Source:  "Controller",
		Subject: supplier.Name,
		Data:    body,
	}))
}

func (supplier *Supplier) MarshalJSON() ([]byte, error) {
	return json.Marshal(*supplier)
}

type Retailer struct {
	Name      string      `json:"-"`
	Nickname  string      `json:"name"`
	Logo      string      `json:"logo"`
	Customers []*Customer `json:"customers"`
}

func (retailer *Retailer) GetPosition() int {
	for ri, r := range airport.Retailers {
		if r.Name == retailer.Name {
			return ri
		}
	}

	return -1
}

func (retailer *Retailer) Disconnect() {
	i := retailer.GetPosition()
	if i != -1 {
		for _, c := range retailer.Customers {
			c.Send("c")
		}
		airport.Retailers = append(airport.Retailers[:i], airport.Retailers[i+1:]...)
		Broadcast(`{"type":"rmretailer","r":` + strconv.Itoa(i) + `}`)
		UpdateJobs()
		PublishDisconnect(retailer.Name)
	}
}

func (retailer *Retailer) MarshalJSON() ([]byte, error) {
	return json.Marshal(*retailer)
}

type CarrierJob struct {
	Retailer string `json:"toLocation"`
	Supplier string `json:"fromLocation"`
}

func (cj *CarrierJob) MarshalJSON() ([]byte, error) {
	return json.Marshal(*cj)
}

type Carrier struct {
	Name string        `json:"name"`
	Logo string        `json:"logo"`
	Jobs []*CarrierJob `json:"jobs"`
}

func (carrier *Carrier) Disconnect() {
	if i := carrier.GetPosition(); i != -1 {
		airport.Carriers = append(airport.Carriers[:i], airport.Carriers[i+1:]...)
		Broadcast(`{"type":"rmcarrier","c":` + strconv.Itoa(i) + `}`)
		UpdateJobs()
		PublishDisconnect(carrier.Name)
	}
}

func (carrier *Carrier) UpdateJob() {
	body, _ := json.Marshal(carrier.Jobs)
	airport.Sender.Send(airport.Context, EventToMessage(&CloudEvent{
		Type:    "Offer.Service.Transport",
		Source:  "Controller",
		Subject: carrier.Name,
		Data:    body,
	}))
}

func (carrier *Carrier) GetPosition() int {
	for ci, c := range airport.Carriers {
		if c.Name == carrier.Name {
			return ci
		}
	}

	return -1
}

func (carrier *Carrier) MarshalJSON() ([]byte, error) {
	return json.Marshal(*carrier)
}

func GetSupplier(name string) *Supplier {
	for _, s := range airport.Suppliers {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func GetRetailer(name string) *Retailer {
	for _, r := range airport.Retailers {
		if r.Name == name {
			return r
		}
	}
	return nil
}

func GetCarrier(name string) *Carrier {
	for _, c := range airport.Carriers {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func HandleFileRequest(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	url := r.URL.Path[1:]
	bytes, err := ioutil.ReadFile(url)
	if err == nil {
		w.Write(bytes)
		return
	} else {
		switch strings.Trim(url, " \n\t\r") {
		case "":
			bytes, err := ioutil.ReadFile("index.html")
			if err == nil {
				w.Write(bytes)
				return
			}
		case "view":
			bytes, err := ioutil.ReadFile("view.html")
			if err == nil {
				w.Write(bytes)
				return
			}
		}
	}

	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("404: \"" + url + "\" not found\n"))
}

func HandleDataRequest(w http.ResponseWriter, r *http.Request) {
	airport.Mutex.RLock()
	bytes, err := json.Marshal(airport)
	airport.Mutex.RUnlock()
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write(bytes)
		return
	}

	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte("500: \"" + err.Error() + "\""))
}

func HandleCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("500: \"" + err.Error() + "\""))
		return
	}

	defer c.Close()

	client := make(chan string)
	var customer *Customer

	go func() {
		for {
			msg, ok := <-client
			if !ok {
				break
			}

			if c.WriteMessage(websocket.TextMessage, []byte(msg)) != nil {
				break
			}
		}
	}()

	for {
		t, message, err := c.ReadMessage()
		if err != nil {
			return
		}

		if t == websocket.TextMessage {
			msg := string(message)
			switch msg[0] {
			case 'i':
				if len(msg) > 1 {
					i := msg[1:]
					if err == nil {
						airport.Mutex.Lock()
					loop:
						for _, r := range airport.Retailers {
							for _, c := range r.Customers {
								if c.Id == i {
									customer = c
									customer.Client = client
									switch customer.State {
									case CUSTOMER_ORDERING:
										customer.Send("o")
									case CUSTOMER_ORDERED:
										customer.Send("w")
									}
									break loop
								}
							}
						}
						airport.Mutex.Unlock()

						if customer == nil {
							c.WriteMessage(websocket.TextMessage, []byte("s"))
						}
					}
				}
			case 'r':
				airport.Mutex.Lock()
				if (customer == nil || customer.State == CUSTOMER_SATISFIED) && len(msg) > 1 {
					i, err := strconv.Atoi(msg[1:])
					if err == nil && i >= 0 && i < len(airport.Retailers) {
						retailer := airport.Retailers[i]

						customer = &Customer{
							Retailer: retailer,
							State:    CUSTOMER_WALKING,
							Id:       uuid.Must(uuid.NewV4()).String(),
							Client:   client,
						}

						customer.Send("i" + customer.Id)
						retailer.Customers = append(retailer.Customers, customer)

						go func() {
							time.Sleep(time.Millisecond * 2000)
							airport.Mutex.Lock()
							customer.State = CUSTOMER_INLINE
							if _, ci := customer.Position(); ci == 0 {
								customer.Order()
							}
							airport.Mutex.Unlock()
						}()

						Broadcast(`{"type":"customer","r":` + strconv.Itoa(i) + `}`)
					}
				}
				airport.Mutex.Unlock()
			case 'j':
				airport.Mutex.RLock()
				ri, ci := customer.Position()
				Broadcast(`{"type":"jump","r":` + strconv.Itoa(ri) + `,"c":` + strconv.Itoa(ci) + `}`)
				airport.Mutex.RUnlock()
			case 'o':
				if len(msg) > 1 {
					airport.Mutex.Lock()
					if i, err := strconv.Atoi(msg[1:]); err == nil && customer.State == CUSTOMER_ORDERING {
						customer.State = CUSTOMER_ORDERED
						airport.Sender.Send(airport.Context, EventToMessage(&CloudEvent{
							Type:    "Order.OrderStatus.OrderReleased",
							Source:  "Passenger",
							Subject: "Customer." + customer.Id,
							Data:    []byte(`{"provider":"` + customer.Retailer.Name + `","orderStatus":"OrderReleased","customer":"Customer.` + customer.Id + `","offer":"` + Sizes[i] + `"}`),
						}))

						go func() {
							time.Sleep(time.Second * 10)
							airport.Mutex.Lock()
							if customer.State != CUSTOMER_SATISFIED {
								customer.Satisfy(SATISFY_OK)
							}
							airport.Mutex.Unlock()
						}()
					}
					airport.Mutex.Unlock()
				}
			case 'e':
				airport.Mutex.Lock()
				airport.Disabled = true
				for _, r := range airport.Retailers {
					for _, c := range r.Customers {
						c.Satisfy(SATISFY_CLOSE)
					}
				}
				airport.Mutex.Unlock()
			case 'd':
				airport.Mutex.Lock()
				airport.Disabled = false
				airport.Mutex.Unlock()
			}
		}
	}
}

func HandleView(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("500: \"" + err.Error() + "\""))
	}

	defer c.Close()

	client := make(chan string, 0xFF)
	clients_mu.Lock()
	clients = append(clients, client)
	clients_mu.Unlock()

	for {
		msg, ok := <-client
		if !ok {
			break
		}

		if err := c.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			clients_mu.Lock()
			for i, ch := range clients {
				if ch == client {
					close(client)
					clients = append(clients[:i], clients[i+1:]...)
					break
				}
			}
			clients_mu.Unlock()
			break
		}
	}
}

func Broadcast(msg string) {
	clients_mu.Lock()
	for _, c := range clients {
		c <- msg
	}
	clients_mu.Unlock()
}

func UpdateJobs() {
	l := len(airport.Suppliers)
	if l == 0 {
		return
	}

	for _, s := range airport.Suppliers {
		s.Jobs = nil
	}

	for _, c := range airport.Carriers {
		c.Jobs = nil
	}

	i := 0
	for _, s := range Sizes {
		for _, r := range airport.Retailers {
			supplier := airport.Suppliers[i%l]

			func() {
				for _, j := range supplier.Jobs {
					if j.Retailer == r.Name {
						j.Offers = append(j.Offers, s)
						return
					}
				}

				supplier.Jobs = append(supplier.Jobs, &SupplierJob{
					Retailer: r.Name,
					Offers:   []string{s},
				})
			}()

			i++
		}
	}

	if l = len(airport.Carriers); l == 0 {
		for _, s := range airport.Suppliers {
			s.UpdateJob()
		}
	} else {
		i = 0
		for _, s := range airport.Suppliers {
			s.UpdateJob()
			for _, r := range s.Jobs {
				c := airport.Carriers[i%l]
				c.Jobs = append(c.Jobs, &CarrierJob{
					Retailer: r.Retailer,
					Supplier: s.Name,
				})
				i++
			}
		}
	}

	for _, c := range airport.Carriers {
		c.UpdateJob()
	}
}

func MessageToEvent(m *amqp.Message) (*CloudEvent, error) {
	event := CloudEvent{}
	if m.Properties.ContentType == "application/cloudevents+json" {
		if len(m.Data) > 0 {
			err := json.Unmarshal(m.Data[0], &event)
			if err != nil {
				return nil, err
			}
		}
	} else {
		event.SpecVersion = GetAMQPHeader(m, "cloudEvents:specversion")
		event.Type = GetAMQPHeader(m, "cloudEvents:type")
		event.Source = GetAMQPHeader(m, "cloudEvents:source")
		event.Subject = GetAMQPHeader(m, "cloudEvents:subject")
		event.ID = GetAMQPHeader(m, "cloudEvents:id")
		event.Time = GetAMQPHeader(m, "cloudEvents:time")
		event.Cause = GetAMQPHeader(m, "cloudEvents:cause")
		event.ContentType = m.Properties.ContentType
		if len(m.Data) > 0 {
			event.Data = m.Data[0]
		}
	}
	return &event, nil
}

func GetAMQPHeader(m *amqp.Message, name string) string {
	if s, ok := m.ApplicationProperties[name].(string); ok {
		return s
	}
	return ""
}

func PublishReset() {
	err := airport.Sender.Send(airport.Context, EventToMessage(&CloudEvent{
		Type:   "Reset",
		Source: "Controller",
	}))
	if err != nil {
		log.Printf("Error on publishing reset: %s\n", err)
	}
}

func PublishDisconnect(name string) {
	fmt.Println("Published timeout disconnect for: " + name)
	airport.Sender.Send(airport.Context, EventToMessage(&CloudEvent{
		Type:    "Disconnect",
		Source:  "Controller",
		Subject: name,
	}))
}

func Listen(queueURL string) {
	// Format: amqp://user:pass@addr/
	exchange := "/exchange/amq.fanout"
	addr, err := url.Parse(queueURL)
	if err != nil {
		log.Fatalf("Error parsing URL(%s): %s", queueURL, err)
	}
	user := addr.User
	password, _ := user.Password()
	addr.User = nil

	for {
		log.Printf("Dialing: %s", addr)
		client, err := amqp.Dial(addr.String(), amqp.ConnSASLPlain(user.Username(), password))
		if err != nil {
			log.Println(err)
			time.Sleep(2 * time.Second)
			continue
		}

		airport.Context = context.Background()

		log.Printf("Creating a new session...\n")
		session, err := client.NewSession()
		if err != nil {
			log.Println(err)
			continue
		}

		log.Printf("Creating a new sender...\n")
		airport.Sender, err = session.NewSender(amqp.LinkTargetAddress(exchange))
		if err != nil {
			log.Println(err)
			continue
		}

		log.Printf("Creating a new receiver...\n")
		receiver, err := session.NewReceiver(amqp.LinkSourceAddress(exchange), amqp.LinkCredit(10))
		if err != nil {
			log.Println(err)
			continue
		}

		PublishReset()

		for {
			m, err := receiver.Receive(airport.Context)
			if err != nil {
				log.Println(err)
				break
			}
			m.Accept()

			event, err := MessageToEvent(m)
			if err != nil {
				log.Println(err)
				continue
			}

			ProcessEvent(event, m)
		}
	}
}

func ProcessEvent(event *CloudEvent, m *amqp.Message) {
	airport.Mutex.Lock()
	if event.Source != "Controller" {
		if event.Source != "Truck" {
			data, _ := json.Marshal(event)
			Broadcast(`{"type":"event","event":` + string(data) + `}`)
		}

		if event.Type == "Reset" {
			for _, r := range airport.Retailers {
				for _, c := range r.Customers {
					c.Satisfy(SATISFY_CLOSE)
				}

				Broadcast(`{"type":"rmretailer","r":0}`)
			}

			for range airport.Suppliers {
				Broadcast(`{"type":"rmsupplier","r":0}`)
			}

			airport.Retailers = nil
			airport.Suppliers = nil
			airport.Carriers = nil
			UpdateJobs()

			PublishReset()
		}
	}

	source := strings.Split(event.Source, ".")
	if len(event.Cause) > 0 {
		if ate, ok := ates[event.Cause]; ok {
			ate.Timer.Stop()
			delete(ates, event.Cause)
		}
	}

	if len(event.Source) > 0 && len(event.ID) > 0 {
		var data map[string]interface{}
		if json.Unmarshal(event.Data, &data) == nil {
		loop:
			for _, t := range TimeoutEvents {
				if t.Type != event.Type || t.Source != source[0] {
					continue loop
				}

				for k, v := range t.Data {
					if v != data[k] {
						continue loop
					}
				}

				var disconnect func()
				switch t.Expect {
				case EXPECT_PROVIDER:
					name, ok := data["provider"].(string)
					if !ok {
						continue loop
					}

					r := GetRetailer(name)
					if r == nil {
						continue loop
					}

					disconnect = r.Disconnect
				case EXPECT_SUPPLIER:
					if GetRetailer(event.Source) == nil {
						continue loop
					}

					var supplier *Supplier
				suppliers:
					for _, s := range airport.Suppliers {
						for _, j := range s.Jobs {
							if j.Retailer == event.Source {
								supplier = s
								break suppliers
							}
						}
					}

					if supplier == nil {
						continue loop
					}

					disconnect = supplier.Disconnect
				case EXPECT_RETAILER:
					name, ok := data["toLocation"].(string)
					if !ok {
						continue loop
					}

					r := GetRetailer(name)
					if r == nil {
						continue loop
					}

					disconnect = r.Disconnect
				case EXPECT_CARRIER:
					retailer, ok := data["toLocation"].(string)
					if !ok || GetRetailer(retailer) == nil {
						continue loop
					}

					supplier, ok := data["fromLocation"].(string)
					if !ok || GetSupplier(supplier) == nil {
						continue loop
					}

					var carrier *Carrier
				carriers:
					for _, c := range airport.Carriers {
						for _, j := range c.Jobs {
							if j.Retailer == retailer && j.Supplier == supplier {
								carrier = c
								break carriers
							}
						}
					}

					if carrier == nil {
						continue loop
					}

					disconnect = carrier.Disconnect
				}

				func(t TimeoutEvent) {
					ate := ActiveTimeoutEvent{
						Message: m,
						Timer: time.AfterFunc(t.Timeout, func() {
							airport.Mutex.Lock()
							fmt.Println("Disconnected due to: " + event.ID)
							disconnect()
							if t.Resend {
								airport.Sender.Send(airport.Context, m)
							}
							delete(ates, event.ID)
							airport.Mutex.Unlock()
						}),
					}
					ates[event.ID] = &ate
				}(t)
			}
		}
	}

	if len(source) > 1 {
		switch source[0] {
		case "Retailer":
			r := GetRetailer(event.Source)
			switch event.Type {
			case "Order.OrderStatus.OrderReleased", "Order.OrderStatus.OrderDelivered":
				var data struct {
					OrderStatus string `json:"orderStatus"`
					Offer       string `json:"offer"`
				}
				if r != nil && json.Unmarshal(event.Data, &data) == nil {
					switch data.OrderStatus {
					case "OrderReleased":
						Broadcast(`{"type":"` + data.Offer + `","r":` + strconv.Itoa(r.GetPosition()) + `,"c":0}`)
					case "OrderDelivered":
						if len(r.Customers) > 0 {
							if c := r.Customers[0]; c.State == CUSTOMER_ORDERED && ("Customer."+c.Id) == event.Subject {
								c.Satisfy(SATISFY_OK)
							}
						}
					}
				}
			case "Connection":
				var data struct {
					Organization string `json:"organization"`
					Logo         string `json:"logo"`
				}
				if r == nil && json.Unmarshal(event.Data, &data) == nil {
					r = &Retailer{Name: event.Source, Nickname: data.Organization, Logo: data.Logo}
					airport.Retailers = append(airport.Retailers, r)
					Broadcast(`{"type":"retailer","logo":"` + r.Logo + `"}`)
					UpdateJobs()
					fmt.Println("Connected retailer: ", r.Name)
				}
			case "Disconnect":
				if r != nil {
					r.Disconnect()
				}
			}
		case "Supplier":
			s := GetSupplier(event.Source)
			switch event.Type {
			case "Connection":
				if s == nil {
					var data struct {
						Logo string `json:"logo"`
					}

					if json.Unmarshal(event.Data, &data) == nil {
						s = &Supplier{Name: event.Source, Logo: data.Logo}
						airport.Suppliers = append(airport.Suppliers, s)
						Broadcast(`{"type":"supplier","logo":"` + s.Logo + `"}`)
						UpdateJobs()
						fmt.Println("Connected supplier: ", s.Name)
					}
				} else {
					s.UpdateJob()
					fmt.Println("Reconnected supplier: ", s.Name)
				}
			case "Disconnect":
				if s != nil {
					s.Disconnect()
				}
			}
		case "Carrier":
			c := GetCarrier(event.Source)
			switch event.Type {
			case "Connection":
				if c == nil {
					var data struct {
						Logo string `json:"logo"`
					}

					if json.Unmarshal(event.Data, &data) == nil {
						c = &Carrier{Name: event.Source, Logo: data.Logo}
						airport.Carriers = append(airport.Carriers, c)
						Broadcast(`{"type":"carrier","logo":"` + c.Logo + `"}`)
						UpdateJobs()
						fmt.Println("Connected carrier: ", c.Name)
					}
				} else {
					c.UpdateJob()
					fmt.Println("Reconnected carrier: ", c.Name)
				}
			case "Disconnect":
				if c != nil {
					c.Disconnect()
				}
			case "TransferAction.ActionStatus.ActiveActionStatus",
				"TransferAction.ActionStatus.CompletedActionStatus":
				var data struct {
					ActionStatus string `json:"actionStatus"`
					FromLocation string `json:"fromLocation"`
					ToLocation   string `json:"toLocation"`
					Offer        string `json:"offer"`
				}
				if json.Unmarshal(event.Data, &data) == nil {
					switch data.ActionStatus {
					case "ActiveActionStatus":
						supplier := GetSupplier(data.FromLocation)
						retailer := GetRetailer(data.ToLocation)
						if supplier != nil && retailer != nil {
							Broadcast(`{"type":"gocarrier","c":` + strconv.Itoa(c.GetPosition()) + `,"s":` + strconv.Itoa(supplier.GetPosition()) + `,"r":` + strconv.Itoa(retailer.GetPosition()) + `}`)
							go func() {
								time.Sleep(4000 * time.Millisecond)
								data.ActionStatus = "ArrivedActionStatus"
								body, _ := json.Marshal(data)
								airport.Sender.Send(airport.Context, EventToMessage(&CloudEvent{
									Type:    "TransferAction.ActionStatus.ArrivedActionStatus",
									Source:  "Controller",
									Subject: event.Subject,
									Data:    body,
								}))
							}()
						}
					case "CompletedActionStatus":
						if retailer := GetRetailer(data.ToLocation); retailer != nil {
							Broadcast(`{"type":"` + data.Offer + `","r":` + strconv.Itoa(retailer.GetPosition()) + `,"c":1}`)
						}
					}
				}
			}
		}
	}
	airport.Mutex.Unlock()
}

func main() {
	var port int
	var addr string
	flag.IntVar(&port, "p", 80, "port")
	flag.StringVar(&addr, "u", "", "AMQP server")
	flag.Parse()

	if addr == "" {
		log.Fatalln("Missing AMQP URL, use the '-u' flag to specify")
	}

	go Listen(addr)

	http.HandleFunc("/", HandleFileRequest)
	http.HandleFunc("/data", HandleDataRequest)
	http.HandleFunc("/ws_view", HandleView)
	http.HandleFunc("/ws_customer", HandleCustomer)

	fmt.Printf("Listening on port %d\n", port)
	if err := http.ListenAndServe(":"+strconv.Itoa(port), nil); err != nil {
		log.Fatalf("Error starting HTTP server:\n\t%v\n", err)
	}
}
