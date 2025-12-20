module github.com/librescoot/pm-service

go 1.22.1

require (
	github.com/librescoot/librefsm v0.3.0
	github.com/librescoot/redis-ipc v0.4.0
	github.com/redis/go-redis/v9 v9.12.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
)

replace github.com/librescoot/librefsm => ../librefsm
