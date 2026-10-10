CREATE TYPE @type@ AS ENUM ('preserved');
CREATE TABLE @members@ (value integer PRIMARY KEY, payload @type@);
INSERT INTO @members@ VALUES (1, 'preserved');
