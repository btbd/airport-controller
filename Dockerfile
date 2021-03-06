FROM golang
RUN mkdir /airport
COPY *go /airport/
WORKDIR /airport
RUN go get -d .
RUN go build -ldflags "-w -extldflags -static" -tags netgo \
		-installsuffix netgo -o /server main.go

FROM ubuntu
RUN mkdir -p /airport/images
WORKDIR /airport
COPY --from=0 /server /airport/server
COPY *html *js /airport/
COPY images/* /airport/images/
COPY banned /banned
CMD /airport/server
