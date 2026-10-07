#include <cassert>
#include <sstream>
#include <string>

#include <tap.hpp>

int main() {
  {
    std::istringstream input(R"({"id":"r1","result":"tasks"})" "\n");
    std::ostringstream output;
    tap::Client client(input, output);
    std::string contents;
    tap::Error error;
    assert(client.read("tasks.json", contents, error));
    assert(contents == "tasks");
    assert(output.str() == R"({"id":"r1","method":"read","path":"tasks.json"})" "\n");
  }
  {
    std::istringstream input(R"({"id":"different","result":"no"})" "\n");
    std::ostringstream output;
    tap::Client client(input, output);
    tap::Reply reply;
    tap::Error error;
    assert(!client.request("tools", nlohmann::json::object(), reply, error));
    assert(error.code == tap::ErrorCode::protocol);
  }
  {
    std::istringstream input("not-json\n");
    std::ostringstream output;
    tap::Client client(input, output);
    tap::Reply reply;
    tap::Error error;
    assert(!client.request("tools", nlohmann::json::object(), reply, error));
    assert(error.code == tap::ErrorCode::protocol);
  }
  {
    std::istringstream input(R"({"id":"r1","refused":"not declared"})" "\n");
    std::ostringstream output;
    tap::Client client(input, output);
    tap::Reply reply;
    tap::Error error;
    assert(!client.request("read", {{"path", "secret"}}, reply, error));
    assert(error.code == tap::ErrorCode::refused && reply.refused == "not declared");
  }
  {
    std::istringstream input(R"({"id":"r1","violation":true,"landed":true,"stderr":"mismatch"})" "\n");
    std::ostringstream output;
    tap::Client client(input, output);
    tap::Reply reply;
    tap::Error error;
    assert(!client.request("call", {{"alias", "lookup"}}, reply, error));
    assert(error.code == tap::ErrorCode::violation && error.landed && reply.violation && reply.landed);
  }
  {
    const std::string message = "the outcome of this write is unknown: an earlier run stopped while it was in progress";
    std::istringstream input(R"({"id":"r1","unknown":true,"refused":")" + message + R"("})" "\n");
    std::ostringstream output;
    tap::Client client(input, output);
    tap::Reply reply;
    tap::Error error;
    assert(!client.request("write", {{"path", "result.txt"}, {"content", "x"}}, reply, error));
    assert(error.code == tap::ErrorCode::unknown && error.detail == message);
    assert(reply.unknown && reply.refused == message);
  }
  {
    std::istringstream input(R"({"id":"r1","result":"{\"count\":3}"})" "\n");
    std::ostringstream output;
    tap::Client client(input, output);
    tap::Reply reply;
    tap::Error error;
    assert(client.call("search", {{"query", "open tasks"}}, reply, error));
    assert(reply.result == R"({"count":3})");
    const auto frame = nlohmann::json::parse(output.str());
    assert(frame["method"] == "call" && frame["alias"] == "search");
    assert(frame["arguments"]["query"] == "open tasks");
  }
  {
    std::istringstream input(R"({"id":"r1","exit":1,"stderr":"connector failed"})" "\n");
    std::ostringstream output;
    tap::Client client(input, output);
    tap::Reply reply;
    tap::Error error;
    assert(!client.call("search", {{"query", "open tasks"}}, reply, error));
    assert(error.code == tap::ErrorCode::failed);
    assert(error.detail == "call failed: connector failed");
    assert(reply.exit == 1 && reply.stderr_text == "connector failed");
  }
  {
    std::istringstream input(R"({"id":"r1","exit":126,"status":429})" "\n");
    std::ostringstream output;
    tap::Client client(input, output);
    tap::Reply reply;
    tap::Error error;
    assert(client.request("exec", {{"command", "test"}}, reply, error));
    assert(reply.exit == 126 && reply.status == 429);
  }
  {
    std::istringstream input(R"({"id":"r1","exit":1,"stderr":"write failed"})" "\n");
    std::ostringstream output;
    tap::Client client(input, output);
    tap::Reply reply;
    tap::Error error;
    assert(client.request("write", {{"path", "result.txt"}, {"content", "x"}}, reply, error));
    assert(reply.exit == 1 && reply.stderr_text == "write failed");
  }
}
