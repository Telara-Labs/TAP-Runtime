#pragma once

#include <cstdint>
#include <iostream>
#include <string>
#include <utility>
#include <vector>

#include <nlohmann/json.hpp>

namespace tap {

enum class ErrorCode { protocol, refused, violation, unknown, failed };

struct Error {
  ErrorCode code = ErrorCode::protocol;
  bool landed = false;
  std::string detail;
};

// Reply preserves broker status and payload fields. Generic request callers
// interpret Exit (including write) and Status (fetch); read/call helpers turn
// nonzero exits into failed errors.
struct Reply {
  std::string id;
  std::string result;
  std::vector<std::string> tools;
  std::string stdout_text;
  std::string stderr_text;
  std::string stdin_text;
  int exit = 0;
  int status = 0;
  std::string refused;
  bool violation = false;
  bool landed = false;
  bool unknown = false;
  bool gated = false;
};

// Client implements ID-matched requests over TAP's newline-delimited JSON
// broker. It has no direct filesystem, network, subprocess, or env access.
class Client {
 public:
  Client(std::istream& input, std::ostream& output) : input_(input), output_(output) {}
  Client() : Client(std::cin, std::cout) {}

  bool request(const std::string& method, const nlohmann::json& fields,
               Reply& reply, Error& error) {
    if (!allowed_method(method)) {
      error = {ErrorCode::protocol, false, "unsupported broker method: " + method};
      return false;
    }
    if (!fields.is_object() || fields.contains("id") || fields.contains("method")) {
      error = {ErrorCode::protocol, false, "request fields must be an object without reserved id/method"};
      return false;
    }
    reply = Reply{};
    const std::string id = "r" + std::to_string(++next_id_);
    nlohmann::json frame = fields;
    frame["id"] = id;
    frame["method"] = method;
    if (!write_frame(frame, error)) return false;

    std::string line;
    if (!std::getline(input_, line)) {
      error = {ErrorCode::protocol, false, "broker closed before replying"};
      return false;
    }
    const auto response = nlohmann::json::parse(line, nullptr, false);
    if (response.is_discarded() || !response.is_object()) {
      error = {ErrorCode::protocol, false, "invalid broker response JSON"};
      return false;
    }
    const auto response_id = response.find("id");
    if (response_id == response.end() || !response_id->is_string() || response_id->get<std::string>() != id) {
      error = {ErrorCode::protocol, false, "broker response ID does not match request ID"};
      return false;
    }
    if (!valid_optional_field(response, "result", is_string_field) ||
        !valid_optional_field(response, "refused", is_string_field) ||
        !valid_optional_field(response, "stdout", is_string_field) ||
        !valid_optional_field(response, "stderr", is_string_field) ||
        !valid_optional_field(response, "stdin", is_string_field) ||
        !valid_optional_field(response, "exit", is_integer_field) ||
        !valid_optional_field(response, "status", is_integer_field) ||
        !valid_optional_field(response, "violation", is_boolean_field) ||
        !valid_optional_field(response, "landed", is_boolean_field) ||
        !valid_optional_field(response, "unknown", is_boolean_field) ||
        !valid_optional_field(response, "gated", is_boolean_field)) {
      error = {ErrorCode::protocol, false, "broker response contains a field with the wrong type"};
      return false;
    }
    reply.id = id;
    reply.refused = string_field(response, "refused");
    reply.violation = boolean_field(response, "violation");
    reply.landed = boolean_field(response, "landed");
    reply.unknown = boolean_field(response, "unknown");
    reply.gated = boolean_field(response, "gated");
    reply.stdout_text = string_field(response, "stdout");
    reply.stderr_text = string_field(response, "stderr");
    reply.stdin_text = string_field(response, "stdin");
    reply.status = integer_field(response, "status");
    reply.exit = integer_field(response, "exit");
    const auto tools = response.find("tools");
    if (tools != response.end() && tools->is_array()) {
      for (const auto& tool : *tools) {
        if (tool.is_string()) reply.tools.push_back(tool.get<std::string>());
      }
    }
    const auto result = response.find("result");
    if (result != response.end()) {
      if (!result->is_string()) {
        error = {ErrorCode::protocol, false, "broker result is not a string"};
        return false;
      }
      reply.result = result->get<std::string>();
    }
    if (reply.unknown) {
      error = {ErrorCode::unknown, false, reply.refused};
      return false;
    }
    if (!reply.refused.empty()) {
      error = {ErrorCode::refused, false, reply.refused};
      return false;
    }
    if (reply.violation) {
      error = {ErrorCode::violation, reply.landed, reply.stderr_text};
      return false;
    }
    return true;
  }

  bool read(const std::string& path, std::string& result, Error& error) {
    if (path.empty()) {
      error = {ErrorCode::protocol, false, "read path must not be empty"};
      return false;
    }
    Reply reply;
    if (!request("read", {{"path", path}}, reply, error)) return false;
    if (reply.exit != 0) {
      error = {ErrorCode::failed, false, "read failed: " + reply.stderr_text};
      return false;
    }
    result = reply.result;
    return true;
  }

  bool call(const std::string& alias, const nlohmann::json& arguments,
            Reply& reply, Error& error) {
    if (alias.empty()) {
      error = {ErrorCode::protocol, false, "tool alias must not be empty"};
      return false;
    }
    if (!arguments.is_object()) {
      error = {ErrorCode::protocol, false, "tool arguments must be an object"};
      return false;
    }
    if (!request("call", {{"alias", alias}, {"arguments", arguments}}, reply, error)) return false;
    if (reply.exit != 0) {
      error = {ErrorCode::failed, false, "call failed: " + reply.stderr_text};
      return false;
    }
    return true;
  }

  bool finish(const std::string& stdout_text, const std::string& stderr_text,
              int exit_code, Error& error) {
    if (exit_code < 0) {
      error = {ErrorCode::protocol, false, "exit code must be non-negative"};
      return false;
    }
    return write_frame({{"method", "return"}, {"stdout", stdout_text},
                        {"stderr", stderr_text}, {"exit", exit_code}}, error);
  }

 private:
  static bool allowed_method(const std::string& method) {
    return method == "tools" || method == "call" || method == "exec" ||
           method == "read" || method == "write" || method == "fetch";
  }

  using FieldCheck = bool (*)(const nlohmann::json&);

  static bool is_string_field(const nlohmann::json& value) { return value.is_string(); }
  static bool is_integer_field(const nlohmann::json& value) { return value.is_number_integer(); }
  static bool is_boolean_field(const nlohmann::json& value) { return value.is_boolean(); }

  static bool valid_optional_field(const nlohmann::json& object, const char* key, FieldCheck check) {
    const auto field = object.find(key);
    return field == object.end() || check(*field);
  }

  static bool boolean_field(const nlohmann::json& value, const char* key) {
    const auto field = value.find(key);
    return field != value.end() && field->is_boolean() && field->get<bool>();
  }

  static std::string string_field(const nlohmann::json& value, const char* key) {
    const auto field = value.find(key);
    return field != value.end() && field->is_string() ? field->get<std::string>() : std::string();
  }

  static int integer_field(const nlohmann::json& value, const char* key) {
    const auto field = value.find(key);
    return field != value.end() && field->is_number_integer() ? field->get<int>() : 0;
  }

  bool write_frame(const nlohmann::json& frame, Error& error) {
    output_ << frame.dump() << '\n' << std::flush;
    if (!output_) {
      error = {ErrorCode::protocol, false, "could not write broker frame"};
      return false;
    }
    return true;
  }

  std::istream& input_;
  std::ostream& output_;
  std::uint64_t next_id_ = 0;
};

}  // namespace tap
