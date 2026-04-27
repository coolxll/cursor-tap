// Minimal Cursor-like JS fixture for runtime protocol loading.
// cursor-tap-proto-begin: e2e_v1.proto
syntax = "proto3";

package e2e.v1;

option go_package = "github.com/burpheart/cursor-tap/internal/e2e/gen/e2e/v1;e2ev1";

message EchoRequest {
  string message = 1;
}

message EchoResponse {
  string message = 1;
}

service EchoService {
  rpc Echo(EchoRequest) returns (EchoResponse) {}
}
// cursor-tap-proto-end

// cursor-tap-proto-begin: aiserver_v1.proto
syntax = "proto3";

package aiserver.v1;

message StreamUnifiedChatRequest {
  string prompt = 1;
}

message StreamUnifiedChatResponse {
  string text = 1;
}

service ChatService {
  rpc StreamUnifiedChat(StreamUnifiedChatRequest) returns (stream StreamUnifiedChatResponse) {}
}
// cursor-tap-proto-end
