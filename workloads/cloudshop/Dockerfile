FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY api ./api
COPY worker ./worker
RUN cd api && go build -o /out/api . && cd ../worker && go build -o /out/worker .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/api /bin/cloudshop-api
COPY --from=build /out/worker /bin/cloudshop-worker
EXPOSE 8081
CMD ["/bin/cloudshop-api"]
