#!/usr/bin/env python3
"""Calculator — safe arithmetic via an AST whitelist. Stdlib only.
Ported from ormcp's calculator.py (recursive AST evaluator).
"""
import ast
import json
import operator
import sys

SAFE_OPS = {
    ast.Add: operator.add,
    ast.Sub: operator.sub,
    ast.Mult: operator.mul,
    ast.Div: operator.truediv,
    ast.FloorDiv: operator.floordiv,
    ast.Mod: operator.mod,
    ast.Pow: operator.pow,
    ast.USub: operator.neg,
}


def _safe_eval(node):
    if isinstance(node, ast.Expression):
        return _safe_eval(node.body)
    if isinstance(node, ast.Constant):
        if isinstance(node.value, (int, float)):
            return node.value
        raise ValueError(f"unsupported constant: {node.value!r}")
    if isinstance(node, ast.BinOp):
        op_type = type(node.op)
        if op_type not in SAFE_OPS:
            raise ValueError(f"unsupported operator: {op_type.__name__}")
        left, right = _safe_eval(node.left), _safe_eval(node.right)
        if op_type is ast.Pow and abs(right) > 10000:
            raise ValueError("exponent too large (max 10000)")
        return SAFE_OPS[op_type](left, right)
    if isinstance(node, ast.UnaryOp):
        op_type = type(node.op)
        if op_type not in SAFE_OPS:
            raise ValueError(f"unsupported unary operator: {op_type.__name__}")
        return SAFE_OPS[op_type](_safe_eval(node.operand))
    raise ValueError(f"unsupported expression: {type(node).__name__}")


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    expression = payload.get("expression", "")
    cleaned = expression.strip().replace("^", "**").replace("×", "*").replace("÷", "/")

    try:
        tree = ast.parse(cleaned, mode="eval")
        result = _safe_eval(tree)
    except (ValueError, SyntaxError, TypeError, ZeroDivisionError) as e:
        print(f"could not evaluate '{expression}': {e}", file=sys.stderr)
        sys.exit(1)

    if isinstance(result, float) and result == int(result):
        result = int(result)

    json.dump({"result": result, "display": f"{cleaned} = {result}"}, sys.stdout)


if __name__ == "__main__":
    main()
