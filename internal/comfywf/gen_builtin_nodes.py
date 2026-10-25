#!/usr/bin/env python3
"""Extract every node type registered by ComfyUI core (no torch import; pure AST).

Usage: python3 -I gen_builtin_nodes.py <ComfyUI checkout dir> > builtin_nodes.txt
Diagnostics (per-file counts, anything unresolved) go to stderr.

Mirrors nodes.py loader logic:
  * nodes.py top-level NODE_CLASS_MAPPINGS dict literal
  * the files listed in nodes.py init_builtin_extra_nodes() `extras_files` (comfy_extras/)
  * the files listed in init_builtin_api_nodes() `api_nodes_files` (comfy_api_nodes/)
  For each module: V1 (module has NODE_CLASS_MAPPINGS) wins; else V3 (comfy_entrypoint ->
  ComfyExtension.get_node_list() -> [io.ComfyNode classes] -> define_schema() io.Schema(node_id=...)).
"""
import ast
import os
import sys


def str_list_assigned(tree, func_name, var_name):
    """Find `var_name = [ "a.py", ... ]` inside async def func_name in nodes.py."""
    for n in ast.walk(tree):
        if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.name == func_name:
            for m in ast.walk(n):
                if isinstance(m, ast.Assign) and any(isinstance(t, ast.Name) and t.id == var_name for t in m.targets):
                    return [e.value for e in m.value.elts]
    raise SystemExit(f"cannot find {var_name} in {func_name}")


def const_str(n):
    return n.value if isinstance(n, ast.Constant) and isinstance(n.value, str) else None


class Module:
    def __init__(self, path):
        self.path = path
        self.tree = ast.parse(open(path, encoding="utf-8").read(), path)
        self.classes = {n.name: n for n in self.tree.body if isinstance(n, ast.ClassDef)}
        self.top_lists = {}
        for n in self.tree.body:
            if isinstance(n, ast.Assign) and isinstance(n.value, ast.List):
                for t in n.targets:
                    if isinstance(t, ast.Name):
                        self.top_lists[t.id] = n.value

    def class_attr(self, cname, attr, seen=None):
        c = self.classes.get(cname)
        if c is None:
            return None
        for st in c.body:
            if isinstance(st, ast.Assign) and any(isinstance(t, ast.Name) and t.id == attr for t in st.targets):
                return const_str(st.value)
        for b in c.bases:  # inherited class attribute
            if isinstance(b, ast.Name):
                v = self.class_attr(b.id, attr)
                if v:
                    return v
        return None

    def schema_node_id(self, cname):
        """node_id=... keyword of a Call inside define_schema (inherit through bases)."""
        c = self.classes.get(cname)
        if c is None:
            return None
        for st in c.body:
            if isinstance(st, (ast.FunctionDef, ast.AsyncFunctionDef)) and st.name == "define_schema":
                # subclass override style: schema = super().define_schema(); schema.node_id = "X"
                for m in ast.walk(st):
                    if (isinstance(m, ast.Assign) and isinstance(m.targets[0], ast.Attribute)
                            and m.targets[0].attr == "node_id"):
                        v = const_str(m.value)
                        if v is None:
                            raise ValueError(f"{self.path}:{cname}: non-constant schema.node_id")
                        return v
                for m in ast.walk(st):
                    if isinstance(m, ast.Call):
                        for kw in m.keywords:
                            if kw.arg == "node_id":
                                v = const_str(kw.value)
                                if v is None:
                                    raise ValueError(f"{self.path}:{cname}: non-constant node_id {ast.dump(kw.value)}")
                                return v
                raise ValueError(f"{self.path}:{cname}: define_schema without node_id")
        for b in c.bases:
            if isinstance(b, ast.Name) and b.id in self.classes:
                return self.schema_node_id(b.id)
        return None

    def v1(self):
        """Returns list of keys, or None when the module has no NODE_CLASS_MAPPINGS."""
        keys, found = [], False
        for n in self.tree.body:
            if isinstance(n, ast.Assign) and any(isinstance(t, ast.Name) and t.id == "NODE_CLASS_MAPPINGS" for t in n.targets):
                found = True
                if not isinstance(n.value, ast.Dict):
                    raise ValueError(f"{self.path}: NODE_CLASS_MAPPINGS not a dict literal")
                for k in n.value.keys:
                    s = const_str(k)
                    if s is None:
                        raise ValueError(f"{self.path}: non-constant key {ast.dump(k)}")
                    keys.append(s)
            # NODE_CLASS_MAPPINGS.update({...})
            if isinstance(n, ast.Expr) and isinstance(n.value, ast.Call):
                f = n.value.func
                if isinstance(f, ast.Attribute) and f.attr == "update" and isinstance(f.value, ast.Name) and f.value.id == "NODE_CLASS_MAPPINGS":
                    d = n.value.args[0]
                    keys += [const_str(k) for k in d.keys]
            # NODE_CLASS_MAPPINGS["X"] = Y
            if isinstance(n, ast.Assign) and isinstance(n.targets[0], ast.Subscript):
                t = n.targets[0]
                if isinstance(t.value, ast.Name) and t.value.id == "NODE_CLASS_MAPPINGS":
                    s = const_str(t.slice)
                    if s is None:
                        raise ValueError(f"{self.path}: dynamic subscript key")
                    keys.append(s)
            # for node in node_list: NODE_CLASS_MAPPINGS[node.NodeId] = node   (nodes_hooks.py)
            if isinstance(n, ast.For) and isinstance(n.iter, ast.Name) and isinstance(n.target, ast.Name):
                for st in n.body:
                    if (isinstance(st, ast.Assign) and isinstance(st.targets[0], ast.Subscript)
                            and isinstance(st.targets[0].value, ast.Name) and st.targets[0].value.id == "NODE_CLASS_MAPPINGS"):
                        sl = st.targets[0].slice
                        if not (isinstance(sl, ast.Attribute) and isinstance(sl.value, ast.Name) and sl.value.id == n.target.id):
                            raise ValueError(f"{self.path}: unsupported loop key")
                        for e in self.top_lists[n.iter.id].elts:
                            v = self.class_attr(e.id, sl.attr)
                            if v is None:
                                raise ValueError(f"{self.path}: {e.id}.{sl.attr} unresolved")
                            keys.append(v)
        return keys if found else None

    def has_entrypoint(self):
        return any(isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.name == "comfy_entrypoint" for n in self.tree.body)

    def v3(self):
        lists = []
        for c in self.classes.values():
            for st in c.body:
                if isinstance(st, (ast.FunctionDef, ast.AsyncFunctionDef)) and st.name == "get_node_list":
                    for m in ast.walk(st):
                        if isinstance(m, ast.Return):
                            if not isinstance(m.value, ast.List):
                                raise ValueError(f"{self.path}: get_node_list returns non-literal")
                            lists.append(m.value)
        if len(lists) != 1:
            raise ValueError(f"{self.path}: expected one get_node_list return, got {len(lists)}")
        out = []
        for e in lists[0].elts:
            if not isinstance(e, ast.Name):
                raise ValueError(f"{self.path}: non-name element {ast.dump(e)}")
            nid = self.schema_node_id(e.id)
            if nid is None:
                raise ValueError(f"{self.path}: cannot resolve node_id of {e.id}")
            out.append(nid)
        return out


def main(root):
    nodes_py = os.path.join(root, "nodes.py")
    tree = ast.parse(open(nodes_py, encoding="utf-8").read())
    files = [("core", nodes_py)]
    files += [("extras", os.path.join(root, "comfy_extras", f)) for f in str_list_assigned(tree, "init_builtin_extra_nodes", "extras_files")]
    files += [("api", os.path.join(root, "comfy_api_nodes", f)) for f in str_list_assigned(tree, "init_builtin_api_nodes", "api_nodes_files")]
    all_types, origin = set(), {}
    counts = {"core": 0, "extras": 0, "api": 0}
    for kind, path in files:
        m = Module(path)
        keys = m.v1()
        style = "v1"
        if keys is None:
            if not m.has_entrypoint():
                print(f"WARN {path}: neither NODE_CLASS_MAPPINGS nor comfy_entrypoint", file=sys.stderr)
                continue
            keys, style = m.v3(), "v3"
        for k in keys:
            if k in all_types:
                print(f"NOTE duplicate {k}: {origin[k]} and {os.path.relpath(path, root)} (later wins at runtime)", file=sys.stderr)
            all_types.add(k)
            origin[k] = os.path.relpath(path, root)
        counts[kind] += len(keys)
        print(f"{os.path.relpath(path, root)}\t{style}\t{len(keys)}", file=sys.stderr)
    print(f"TOTAL unique={len(all_types)} counts={counts}", file=sys.stderr)
    for t in sorted(all_types):
        print(t)


if __name__ == "__main__":
    main(sys.argv[1])
