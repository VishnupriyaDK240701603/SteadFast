.PHONY: up down clean test lint scan seed demo chaos verdict bench rehearse mutation-check

up:
	go run ./cmd/ctl up

down:
	go run ./cmd/ctl down

clean:
	go run ./cmd/ctl clean

test:
	go run ./cmd/ctl test

lint:
	go run ./cmd/ctl lint

scan:
	go run ./cmd/ctl scan

seed:
	go run ./cmd/ctl seed

demo:
	go run ./cmd/ctl demo

chaos:
	go run ./cmd/ctl chaos

verdict:
	go run ./cmd/ctl verdict

bench:
	go run ./cmd/ctl bench

rehearse:
	go run ./cmd/ctl rehearse

mutation-check:
	go run ./cmd/ctl mutation-check
