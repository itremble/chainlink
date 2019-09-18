module.exports = port => `{
  "initiators": [{"type": "web"}],
  "tasks": [
    {"type": "httpget", "params": {"get": "http://localhost:${port}"}},
    {"type": "jsonparse", "params": {"path": ["last"]}},
    {
      "type": "ethtx",
      "confirmations": 0,
      "params": {
        "address": "0xaa664fa2fdc390c662de1dbacf1218ac6e066ae6",
        "functionSelector": "setBytes(bytes32,bytes)"
      }
    }
  ]
}`
