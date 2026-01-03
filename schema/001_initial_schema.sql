-- WebSocket Demo - Message Persistence Schema
-- Phase 1: Basic message storage with 30-day retention

-- Users table
CREATE TABLE IF NOT EXISTS users (
    user_id VARCHAR(64) PRIMARY KEY,
    username VARCHAR(255) UNIQUE NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    last_seen TIMESTAMP
);

-- Messages table
CREATE TABLE IF NOT EXISTS messages (
    message_id VARCHAR(64) PRIMARY KEY,
    from_user_id VARCHAR(64) REFERENCES users(user_id),
    to_user_id VARCHAR(64) REFERENCES users(user_id),
    content TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    expires_at TIMESTAMP DEFAULT NOW() + INTERVAL '30 days'
);

-- Message delivery tracking
CREATE TABLE IF NOT EXISTS message_delivery (
    message_id VARCHAR(64) REFERENCES messages(message_id) ON DELETE CASCADE,
    recipient_id VARCHAR(64) REFERENCES users(user_id),
    status VARCHAR(20) DEFAULT 'sent',
    delivered_at TIMESTAMP,
    read_at TIMESTAMP,
    PRIMARY KEY (message_id, recipient_id)
);

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_messages_to ON messages(to_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_from ON messages(from_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_delivery_status ON message_delivery(recipient_id, status);
CREATE INDEX IF NOT EXISTS idx_messages_expires ON messages(expires_at);

-- Auto-create user on insert if not exists
CREATE OR REPLACE FUNCTION ensure_user_exists()
RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO users (user_id, username)
    VALUES (NEW.from_user_id, NEW.from_user_id)
    ON CONFLICT (user_id) DO NOTHING;
    
    IF NEW.to_user_id IS NOT NULL THEN
        INSERT INTO users (user_id, username)
        VALUES (NEW.to_user_id, NEW.to_user_id)
        ON CONFLICT (user_id) DO NOTHING;
    END IF;
    
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS ensure_users_before_message ON messages;
CREATE TRIGGER ensure_users_before_message
BEFORE INSERT ON messages
FOR EACH ROW
EXECUTE FUNCTION ensure_user_exists();

-- Function to clean up expired messages (can be called by cron)
CREATE OR REPLACE FUNCTION cleanup_expired_messages()
RETURNS TABLE(deleted_count BIGINT) AS $$
DECLARE
    count BIGINT;
BEGIN
    DELETE FROM messages WHERE expires_at < NOW();
    GET DIAGNOSTICS count = ROW_COUNT;
    RETURN QUERY SELECT count;
END;
$$ LANGUAGE plpgsql;

-- Grant permissions (adjust as needed for your setup)
-- GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO your_user;
-- GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO your_user;
