<h1 align="center">govd</h1>
<p align="center">
  <a href="https://t.me/">
    <img alt="govd" title="govd" src="https://i.imgur.com/x1SXe1o.png" width="450">
  </a>
</p>

<p align="center">
  extremely lightweight downloader, inside a telegram bot.
</p>

## 🚀 EGOVD

**EGOVD** is a lightweight, fast, and highly configurable Telegram bot designed to download media from multiple platforms directly inside Telegram. 📲

Simply send a link to the bot and let EGOVD handle the rest: content detection, extraction, download, and delivery in the best available quality. ⚡

## ✨ Main Features

* 📥 Download videos, images, audio, and other media
* 🌐 Support for a wide range of websites and extractors
* ⚡ Fast performance with minimal resource usage
* 🧠 Approximately **80 MB of RAM usage**
* 💾 Lightweight installation requiring around **150 MB of disk space**
* 🐳 Easy deployment with **Docker**
* 🤖 Works in private chats, groups, and inline mode
* 🔐 Supports authentication and cookies for compatible extractors
* 🖥️ Compatible with self-hosted Telegram Bot API servers
* 🌍 Translation-ready with internationalization support
* 🛠️ Highly configurable and easy to extend

## 🎯 How It Works

1. 🔗 Send a media link to the bot
2. 🔍 EGOVD automatically analyzes the page
3. 📦 The content is processed and downloaded
4. 📲 The file is sent directly through Telegram

## 🐳 Easy to Deploy

EGOVD is designed for simple Docker deployment, making installation and updates easy on home servers, VPS environments, and self-hosted systems.

## 🔐 Extractor Cookies

Some sites hand back incomplete data to anonymous requests. Instagram, for one, leaves `video_url` out of reels unless the request carries a session, so only the cover image comes through. 🍪

To authenticate an extractor, export that site's cookies in Netscape format (any "cookies.txt" browser extension does it) and save them as `private/cookies/<extractor_id>.txt` — for example `private/cookies/instagram.txt`. The folder is gitignored and already mounted into the container.

Cookies are parsed once and kept in memory, so **restart the container after replacing a file**. They are only ever sent to the domain they declare, so a session is never passed on to the third-party services an extractor may fall back to. 🛡️

> [!WARNING]
> Downloading automatically through a logged-in account can be read as unusual activity and get it checkpointed or suspended. Use a secondary account.

## 💙 Project Goal

The goal of EGOVD is to provide a simple, lightweight, and reliable Telegram downloader that brings multiple media download services together in one easy-to-use bot.

> 📡 One link, one message, your content.
