FROM golang:1.17 AS build
WORKDIR /src

ENV CGO_ENABLED=0

COPY . .
RUN go mod vendor
RUN make

FROM debian
COPY --from=build /src/stonelb /stonelb

ENTRYPOINT ["/stonelb"]