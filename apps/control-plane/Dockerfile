FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN go build -o /out/control-plane .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/control-plane /bin/control-plane
EXPOSE 8080
CMD ["/bin/control-plane"]
