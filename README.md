# Voice server for comm.sqrll.net (designed as microservice)

##### Light server for voice streaming using web. 
###### Frontend available at https://github.com/Przemek2122/voice.sqrll.net

##### Initial setup: (os env)
###### SQRLL_VOICE_PORT – Change port which this server will run on.
###### SQRLL_VOICE_API_KEY – Key for server to access sensitive functions (create room, etc)

##### API (api.go):
###### /api/rooms/create - Create new room (Requires APIKey)
###### /api/rooms/check - Check if room exists
###### /api/rooms/stream - Stream audio data to/from room
