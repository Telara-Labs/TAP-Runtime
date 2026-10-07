#include <iostream>
#include <string>
#include <vector>

#include <nlohmann/json.hpp>
#include <tap/tap.hpp>

using json = nlohmann::json;

bool requested_status(int argc, char** argv, std::string& status, std::string& error) {
  if (argc != 2) {
    error = "expected one JSON input";
    return false;
  }
  const auto input = json::parse(argv[1], nullptr, false);
  if (input.is_discarded()) {
    error = "invalid input JSON";
    return false;
  }
  if (!input.is_object() || input.size() != 1 || !input.contains("status") ||
      !input["status"].is_string()) {
    error = "expected only status";
    return false;
  }
  status = input["status"].get<std::string>();
  if (status != "open" && status != "done") {
    error = "expected status open or done";
    return false;
  }
  return true;
}

bool summarize(tap::Client& client, const std::string& status,
               std::string& output, std::string& error) {
  std::string raw;
  tap::Error broker_error;
  if (!client.read("examples/languages/fixtures/tasks.json", raw, broker_error)) {
    error = broker_error.detail;
    return false;
  }
  const auto tasks = json::parse(raw, nullptr, false);
  if (tasks.is_discarded() || !tasks.is_array()) {
    error = "invalid task array";
    return false;
  }

  json items = json::array();
  for (const auto& row : tasks) {
    if (!row.is_object() || !row.contains("id") || !row["id"].is_string() ||
        !row.contains("title") || !row["title"].is_string() ||
        !row.contains("status") || !row["status"].is_string()) {
      error = "invalid task row";
      return false;
    }
    const auto row_status = row["status"].get<std::string>();
    if (row_status != "open" && row_status != "done") {
      error = "invalid task row";
      return false;
    }
    if (row_status == status) {
      items.push_back({{"id", row["id"]}, {"title", row["title"]}});
    }
  }
  output = json{{"status", status}, {"count", items.size()}, {"items", items}}.dump();
  return true;
}

int main(int argc, char** argv) {
  tap::Client client;
  std::string output;
  std::string error_text;
  std::string status;
  int exit_code = 0;
  if (!requested_status(argc, argv, status, error_text) ||
      !summarize(client, status, output, error_text)) {
    exit_code = 1;
  }
  tap::Error finish_error;
  if (!client.finish(output, error_text, exit_code, finish_error)) return 1;
  return 0;
}
