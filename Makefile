all: server

IMAGE=duglin/airport-controller
PASSWORD?=$(USER)

server: *.go
	go fmt
	go build -ldflags "-w -extldflags -static" -tags netgo \
		-installsuffix netgo -o server main.go

push: .push
.push: server *html *js Dockerfile
	docker build -t $(IMAGE) .
	docker push $(IMAGE)
	touch .push

run: server
	./server -p 93 -u amqp://$(USER):$(PASSWORD)@srcdog.com:9999/
	# ./server -p 93 -u amqp://$(USER):$(PASSWORD)@localhost:9999/

rabbitmq:
	docker run -d -p 9999:5672 --hostname cerabbitmq --name rabbitmq \
		-e RABBITMQ_DEFAULT_USER=cedemo -e RABBITMQ_DEFAULT_PASS=cedemo \
		deissnerk/rabbitmq
	sleep 10
	docker exec -ti rabbitmq rabbitmqctl set_policy TTL ".*" \
		'{"message-ttl":60000,"expires":120000}' --apply-to all

clean:
	rm -f server .push
	docker rm -f rabbitmq
