-- 为可枚举摄取源增加稳定名字，使全量扫描能判断哪些来源文件已经消失。
-- 旧摄取数据保持 NULL：source_key 是单向哈希，无法可靠反推出原 source label。
ALTER TABLE cards
    ADD COLUMN source_name VARCHAR(120) NULL AFTER source,
    ADD INDEX idx_cards_source_name (space_id, source_name, status);
