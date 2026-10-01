"""Recognize one exact Python file transform. Parse only; never evaluate input."""

import ast
import sys


def name(node):
    return node.id if isinstance(node, ast.Name) else None


def text(node):
    return node.value if isinstance(node, ast.Constant) and isinstance(node.value, str) else None


def call(node, method):
    return isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) and node.func.attr == method and not node.keywords


def strict(source):
    try:
        tree = ast.parse(source)
    except SyntaxError:
        return False
    if len(tree.body) != 4:
        return False
    path_assign, read_assign, replace_assign, write_expr = tree.body
    if not (isinstance(path_assign, ast.Assign) and len(path_assign.targets) == 1 and name(path_assign.targets[0]) and text(path_assign.value) is not None):
        return False
    path_var = name(path_assign.targets[0])
    if not (isinstance(read_assign, ast.Assign) and len(read_assign.targets) == 1 and name(read_assign.targets[0]) and call(read_assign.value, "read") and not read_assign.value.args):
        return False
    opened = read_assign.value.func.value
    if not (isinstance(opened, ast.Call) and name(opened.func) == "open" and len(opened.args) == 1 and name(opened.args[0]) == path_var and not opened.keywords):
        return False
    data_var = name(read_assign.targets[0])
    if data_var == path_var or path_var == "open":
        return False
    if not (isinstance(replace_assign, ast.Assign) and len(replace_assign.targets) == 1 and name(replace_assign.targets[0]) and call(replace_assign.value, "replace") and len(replace_assign.value.args) == 2 and name(replace_assign.value.func.value) == data_var and all(text(arg) is not None for arg in replace_assign.value.args)):
        return False
    if text(replace_assign.value.args[0]) == "":
        return False
    changed_var = name(replace_assign.targets[0])
    if changed_var == path_var:
        return False
    if not (isinstance(write_expr, ast.Expr) and call(write_expr.value, "write") and len(write_expr.value.args) == 1 and name(write_expr.value.args[0]) == changed_var):
        return False
    target = write_expr.value.func.value
    return isinstance(target, ast.Call) and name(target.func) == "open" and len(target.args) == 2 and name(target.args[0]) == path_var and text(target.args[1]) == "w" and not target.keywords


if __name__ == "__main__":
    print("yes" if strict(sys.stdin.read()) else "no")
