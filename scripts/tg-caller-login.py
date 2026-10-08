"""Log Jasmine's Telegram calling account in once and print its session string.

Run it on your own computer (it asks for the login code Telegram sends you):

    ~/.local/bin/uv run --with telethon python scripts/tg-caller-login.py

Get TG_API_ID and TG_API_HASH from https://my.telegram.org -> API development tools,
logged in with the account Jasmine will call from. Put the printed TG_CALLER_SESSION on
Render. Anyone holding that string controls the account, so treat it like a password.
"""

import asyncio
import getpass

from telethon import TelegramClient
from telethon.sessions import StringSession


async def main():
    api_id = int(input("TG_API_ID: ").strip())
    api_hash = input("TG_API_HASH: ").strip()
    client = TelegramClient(StringSession(), api_id, api_hash)
    await client.start(
        phone=lambda: input("Phone number of Jasmine's account (with country code): ").strip(),
        code_callback=lambda: input("Login code Telegram just sent: ").strip(),
        password=lambda: getpass.getpass("Two-step verification password (blank if none): "),
    )
    me = await client.get_me()
    print(f"\nLogged in as {me.first_name} (@{me.username}). Add these on Render:\n")
    print(f"TG_API_ID={api_id}")
    print(f"TG_API_HASH={api_hash}")
    print(f"TG_CALLER_SESSION={client.session.save()}")
    await client.disconnect()


asyncio.run(main())
