CREATE DATABASE `webmail` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
CREATE USER 'webmail' @'localhost' IDENTIFIED BY 'webmail';
GRANT ALL PRIVILEGES ON `webmail`.* TO 'webmail' @'localhost' WITH
GRANT OPTION;
FLUSH PRIVILEGES;
