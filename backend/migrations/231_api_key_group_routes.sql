-- 231. API Key 多分组路由表
-- 允许一个 API Key 绑定多个分组并设置优先级。
-- 路由规则见 service/api_key_multi_group_route.go：
--   1. 高优先级（priority 数值小）分组优先使用；
--   2. 高优先级分组不可用时，降级到下一优先级分组；
--   3. 同一优先级内按会话 ID 稳定选择（会话粘性）；
--   4. 密钥列表只展示优先级最高分组中的第一个分组。
-- 与 api_keys.group_id（单分组，向后兼容）并存：未配置多分组路由时
-- 仍走原单分组字段；配置了多分组路由时该路由表生效。
CREATE TABLE IF NOT EXISTS api_key_group_routes (
    key_id      BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    group_id    BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    priority    INT    NOT NULL,                          -- 越小越优先（1 最高）
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (key_id, group_id)
);

CREATE INDEX IF NOT EXISTS idx_api_key_group_routes_key_id   ON api_key_group_routes(key_id);
CREATE INDEX IF NOT EXISTS idx_api_key_group_routes_group_id ON api_key_group_routes(group_id);
CREATE INDEX IF NOT EXISTS idx_api_key_group_routes_priority ON api_key_group_routes(priority);
