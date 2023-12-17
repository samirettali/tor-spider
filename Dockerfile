FROM golang:alpine AS build

RUN apk add --no-cache git

ENV GO111MODULE=on \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /build

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .

RUN go build .

FROM scratch AS bin

WORKDIR /dist

COPY --from=build /build/tor-spider .
COPY blacklist.txt .

# Using a blacklist in order to prevend ending up crawling huge sites like 
# facebook and pornhub
CMD ["./tor-spider"]
