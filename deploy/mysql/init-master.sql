-- deploy/mysql/init-master.sql
-- Mounted at /docker-entrypoint-initdb.d/ on the master container.
-- Executed ONCE when the master data directory is first initialised.
-- Creates a dedicated replication-only user with minimal privileges.

-- Replication user: repl / repl123
-- Allowed from any host inside the docker network.
-- REPLICATION SLAVE is the minimum privilege required.
CREATE USER IF NOT EXISTS 'repl'@'%' IDENTIFIED WITH mysql_native_password BY 'repl123';
GRANT REPLICATION SLAVE ON *.* TO 'repl'@'%';
FLUSH PRIVILEGES;
