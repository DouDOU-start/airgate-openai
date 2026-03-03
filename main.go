package main

import sdkgrpc "github.com/DouDOU-start/airgate-sdk/grpc"

func main() {
	sdkgrpc.Serve(&OpenAIGateway{})
}
