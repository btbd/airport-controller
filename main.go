package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	uuid "github.com/satori/go.uuid"
	"github.com/streadway/amqp"
)

var airport struct {
	Suppliers []*Supplier   `json:"suppliers"`
	Retailers []*Retailer   `json:"retailers"`
	Carriers  []*Carrier    `json:"carriers"`
	Mutex     sync.RWMutex  `json:"-"`
	Channel   *amqp.Channel `json:"-"`
}

var upgrader = websocket.Upgrader{}
var clients = []chan string{}
var clients_mu *sync.Mutex = &sync.Mutex{}
var ates = map[string]*ActiveTimeoutEvent{}

var Sizes = []string{"small", "medium", "large"}
var TimeoutEvents = []TimeoutEvent{
	{
		Type:    "Order",
		Source:  "Passenger",
		Timeout: 10 * time.Second,
		Data: map[string]interface{}{
			"orderStatus": "OrderReleased",
		},
		Expect: EXPECT_PROVIDER,
		Resend: true,
	},
	{
		Type:    "Order",
		Source:  "Retailer",
		Timeout: 10 * time.Second,
		Data: map[string]interface{}{
			"orderStatus": "OrderReleased",
		},
		Expect: EXPECT_SUPPLIER,
		Resend: true,
	},
	{
		Type:    "TransferAction",
		Source:  "Supplier",
		Timeout: 10 * time.Second,
		Data: map[string]interface{}{
			"actionStatus": "PotentialActionStatus",
		},
		Expect: EXPECT_CARRIER,
		Resend: true,
	},
	{
		Type:    "TransferAction",
		Source:  "Controller",
		Timeout: 10 * time.Second,
		Data: map[string]interface{}{
			"actionStatus": "ArrivedActionStatus",
		},
		Expect: EXPECT_CARRIER,
		Resend: true,
	},
	{
		Type:    "TransferAction",
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
	EXPECT_PROVIDER = iota // expect response from provider in data
	EXPECT_SUPPLIER = iota // expect response from supplier that hanldes retailer (from source)
	EXPECT_RETAILER = iota // expect response from retailer (from tolocation)
	EXPECT_CARRIER  = iota // expect response from carrier that handles retailer and supplier (from tolocation and fromlocation)
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
	Message amqp.Delivery
	Timer   *time.Timer
}

const (
	CUSTOMER_WALKING   = iota
	CUSTOMER_INLINE    = iota
	CUSTOMER_ORDERING  = iota
	CUSTOMER_ORDERED   = iota
	CUSTOMER_SATISFIED = iota
)

type Customer struct {
	Retailer *Retailer   `json:"-"`
	State    int         `json:"-"`
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
			customer.Satisfy(true)
		}
		airport.Mutex.Unlock()
	}()
}

func (customer *Customer) Satisfy(forced bool) {
	ri, ci := customer.Position()
	if ri == -1 || ci == -1 {
		return
	}

	customer.State = CUSTOMER_SATISFIED
	if forced {
		customer.Send("f")
	} else {
		customer.Send("s")
	}

	var customers []*Customer
	if c := customer.Retailer.Customers; len(c) > 1 {
		customers = c[1:]
		customers[0].Order()
	}

	customer.Retailer.Customers = customers
	Broadcast(`{"type":"satisfied","r":` + strconv.Itoa(ri) + `,"c":` + strconv.Itoa(ci) + `}`)
}

func (customer *Customer) MarshalJSON() ([]byte, error) {
	return json.Marshal(*customer)
}

type SupplierJob struct {
	Retailer string   `json:"customer"`
	Sizes    []string `json:"offer"`
}

func (sj *SupplierJob) MarshalJSON() ([]byte, error) {
	return json.Marshal(*sj)
}

type Supplier struct {
	Name string         `json:"name"`
	Jobs []*SupplierJob `json:"-"`
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

func (supplier *Supplier) UpdateJob() {
	body, _ := json.Marshal(supplier.Jobs)
	airport.Channel.Publish("airport", "", false, false, amqp.Publishing{
		Headers: amqp.Table{
			"ce-specversion": "0.3",
			"ce-type":        "Offer.Product",
			"ce-source":      "Controller",
			"ce-subject":     supplier.Name,
			"ce-id":          uuid.Must(uuid.NewV4()).String(),
			"ce-time":        time.Now().Format(time.RFC3339),
		},
		ContentType: "application/json",
		Body:        body,
	})
}

func (supplier *Supplier) MarshalJSON() ([]byte, error) {
	return json.Marshal(*supplier)
}

type Retailer struct {
	Name      string      `json:"-"`
	Nickname  string      `json:"name"`
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
			c.Send("s")
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
	Jobs []*CarrierJob `json:"-"`
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
	airport.Channel.Publish("airport", "", false, false, amqp.Publishing{
		Headers: amqp.Table{
			"ce-specversion": "0.3",
			"ce-type":        "Offer.Service.Transport",
			"ce-source":      "Controller",
			"ce-subject":     carrier.Name,
			"ce-id":          uuid.Must(uuid.NewV4()).String(),
			"ce-time":        time.Now().Format(time.RFC3339),
		},
		ContentType: "application/json",
		Body:        body,
	})
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

	id := ""
	client := make(chan string)
	customer := &Customer{}

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
									id = i
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

						if id == "" {
							c.WriteMessage(websocket.TextMessage, []byte("s"))
						}
					}
				}
			case 'r':
				if len(msg) > 1 {
					i, err := strconv.Atoi(msg[1:])
					airport.Mutex.Lock()
					if err == nil && i >= 0 && i < len(airport.Retailers) {
						id = uuid.Must(uuid.NewV4()).String()
						retailer := airport.Retailers[i]

						*customer = Customer{
							Retailer: retailer,
							State:    CUSTOMER_WALKING,
							Id:       id,
							Client:   client,
						}

						customer.Send("i" + id)
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
					airport.Mutex.Unlock()
				}
			case 'j':
				airport.Mutex.RLock()
				ri, ci := customer.Position()
				airport.Mutex.RUnlock()
				Broadcast(`{"type":"jump","r":` + strconv.Itoa(ri) + `,"c":` + strconv.Itoa(ci) + `}`)
			case 'o':
				if len(msg) > 1 {
					i, err := strconv.Atoi(msg[1:])
					if err == nil {
						airport.Mutex.Lock()
						if customer.State == CUSTOMER_ORDERING {
							customer.State = CUSTOMER_ORDERED
							airport.Channel.Publish("airport", "", false, false, amqp.Publishing{
								Headers: amqp.Table{
									"ce-specversion": "0.3",
									"ce-type":        "Order",
									"ce-source":      "Passenger",
									"ce-subject":     "Customer." + id,
									"ce-id":          uuid.Must(uuid.NewV4()).String(),
									"ce-time":        time.Now().Format(time.RFC3339),
								},
								ContentType: "application/json",
								Body:        []byte(`{"provider":"` + customer.Retailer.Name + `","orderStatus":"OrderReleased","customer":"Customer.` + id + `","offer":"` + Sizes[i] + `"}`),
							})
						}
						airport.Mutex.Unlock()
					}
				}
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
						j.Sizes = append(j.Sizes, s)
						return
					}
				}

				supplier.Jobs = append(supplier.Jobs, &SupplierJob{
					Retailer: r.Name,
					Sizes:    []string{s},
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

func DeliveryToEvent(d *amqp.Delivery) (*CloudEvent, error) {
	event := CloudEvent{}
	if d.ContentType == "application/cloudevents+json" {
		err := json.Unmarshal(d.Body, &event)
		if err != nil {
			return nil, err
		}
	} else {
		event.SpecVersion = GetAMQPHeader(d, "ce-specversion")
		event.Type = GetAMQPHeader(d, "ce-type")
		event.Source = GetAMQPHeader(d, "ce-source")
		event.Subject = GetAMQPHeader(d, "ce-subject")
		event.ID = GetAMQPHeader(d, "ce-id")
		event.Time = GetAMQPHeader(d, "ce-time")
		event.Cause = GetAMQPHeader(d, "ce-cause")
		event.ContentType = d.ContentType
		event.Data = d.Body
	}
	return &event, nil
}

func GetAMQPHeader(d *amqp.Delivery, name string) string {
	if s, ok := d.Headers[name].(string); ok {
		return s
	}
	return ""
}

func PublishReset() {
	airport.Channel.Publish("airport", "", false, false, amqp.Publishing{
		Headers: amqp.Table{
			"ce-specversion": "0.3",
			"ce-type":        "Reset",
			"ce-source":      "Controller",
			"ce-id":          uuid.Must(uuid.NewV4()).String(),
			"ce-time":        time.Now().Format(time.RFC3339),
		},
		ContentType: "application/json",
	})
}

func PublishDisconnect(name string) {
	fmt.Println("Published timeout disconnect for: " + name)
	airport.Channel.Publish("airport", "", false, false, amqp.Publishing{
		Headers: amqp.Table{
			"ce-specversion": "0.3",
			"ce-type":        "Disconnect",
			"ce-source":      "Controller",
			"ce-subject":     name,
			"ce-id":          uuid.Must(uuid.NewV4()).String(),
			"ce-time":        time.Now().Format(time.RFC3339),
		},
	})
}

func Listen(addr string) {
	conn, err := amqp.Dial(addr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	airport.Channel, err = conn.Channel()
	if err != nil {
		log.Fatal(err)
	}

	err = airport.Channel.ExchangeDeclare("airport", "topic", true, false, false, false, nil)
	if err != nil {
		log.Fatal(err)
	}

	q, err := airport.Channel.QueueDeclare("", false, false, false, false, nil)
	if err != nil {
		log.Fatal(err)
	}

	err = airport.Channel.QueueBind(q.Name, "#", "airport", false, nil)
	if err != nil {
		log.Fatal(err)
	}

	msgs, err := airport.Channel.Consume(q.Name, "", true, false, false, false, nil)
	if err != nil {
		log.Fatal(err)
	}

	PublishReset()

	for d := range msgs {
		event, err := DeliveryToEvent(&d)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			continue
		}

		airport.Mutex.Lock()
		if event.Source != "Controller" {
			if event.Source != "Truck" {
				data, _ := json.Marshal(event)
				Broadcast(`{"type":"event","event":` + string(data) + `}`)
			}

			if event.Type == "Reset" {
				for _, r := range airport.Retailers {
					for _, c := range r.Customers {
						c.Satisfy(false)
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
			if json.Unmarshal(d.Body, &data) == nil {
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
							Message: d,
							Timer: time.AfterFunc(t.Timeout, func() {
								airport.Mutex.Lock()
								fmt.Println("Disconnected due to: " + event.ID)
								disconnect()
								if t.Resend {
									airport.Channel.Publish("airport", "", false, false, amqp.Publishing{
										Headers:         d.Headers,
										ContentType:     d.ContentType,
										ContentEncoding: d.ContentEncoding,
										Body:            d.Body,
									})
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
				case "Order":
					var data struct {
						OrderStatus string `json:"orderStatus"`
						Offer       string `json:"offer"`
					}
					if r != nil && json.Unmarshal(d.Body, &data) == nil {
						switch data.OrderStatus {
						case "OrderReleased":
							Broadcast(`{"type":"` + data.Offer + `","r":` + strconv.Itoa(r.GetPosition()) + `,"c":0}`)
						case "OrderDelivered":
							if len(r.Customers) > 0 {
								r.Customers[0].Satisfy(false)
							}
						}
					}
				case "Connection":
					var data struct {
						Organization string `json:"organization"`
					}
					if r == nil && json.Unmarshal(d.Body, &data) == nil {
						r = &Retailer{Name: event.Source, Nickname: data.Organization}
						airport.Retailers = append(airport.Retailers, r)
						Broadcast(`{"type":"retailer"}`)
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
						s = &Supplier{Name: event.Source}
						airport.Suppliers = append(airport.Suppliers, s)
						Broadcast(`{"type":"supplier"}`)
						UpdateJobs()
						fmt.Println("Connected supplier: ", s.Name)
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
						c = &Carrier{Name: event.Source}
						airport.Carriers = append(airport.Carriers, c)
						Broadcast(`{"type":"carrier"}`)
						UpdateJobs()
						fmt.Println("Connected carrier: ", c.Name)
					} else {
						c.UpdateJob()
						fmt.Println("Reconnected carrier: ", c.Name)
					}
				case "Disconnect":
					if c != nil {
						c.Disconnect()
					}
				case "TransferAction":
					var data struct {
						ActionStatus string `json:"actionStatus"`
						FromLocation string `json:"fromLocation"`
						ToLocation   string `json:"toLocation"`
						Offer        string `json:"offer"`
					}
					if json.Unmarshal(d.Body, &data) == nil {
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
									airport.Channel.Publish("airport", "", false, false, amqp.Publishing{
										Headers: amqp.Table{
											"ce-specversion": "0.3",
											"ce-type":        "TransferAction",
											"ce-source":      "Controller",
											"ce-subject":     event.Subject,
											"ce-id":          uuid.Must(uuid.NewV4()).String(),
											"ce-time":        time.Now().Format(time.RFC3339),
										},
										ContentType: "application/json",
										Body:        body,
									})
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
}

func main() {
	var port int
	var addr string
	flag.IntVar(&port, "p", 80, "port")
	flag.StringVar(&addr, "u", "", "AMQP server")
	flag.Parse()

	if addr == "" {
		log.Fatal("Missing AMQP URL, use the '-u' flat to specify\n")
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
