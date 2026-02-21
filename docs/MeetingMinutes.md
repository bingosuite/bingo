# Existing Solutions Analysis

## Meeting 1: January 19th, 2025

- We met with Robert Laganière. He agreed to be our client for this project. He is a professor who teaches programming paradigms at the University of Ottawa.
- He is an expert at Go and has a lot of experience working with idiomatic patterns in go, especially in concurrent programming.
- We pitched our ideas and trajectory for the project. He seems onboard and is excited to work with us. We agreed to have bi-weekly meetings to discuss our progress and get feedback from him.
- We discussed the existing solutions for Go concurrency debugging. Robert mentioned that while there are tools like `go tool trace` and delve, they lack interactivity and a concurrency-focused approach.
- We brainstormed features that would be beneficial for our tool, such as visualizing goroutine interactions, detecting common concurrency bugs, and providing an intuitive UI for developers.

## Meeting 2: February 9th, 2025

- Second meeting with Robert Laganière. We presented our plan for the architecture of our debugger and did some system design together on a collaborative whiteboard. We used to opportunity to ask questions about idiomatic patterns in go, such as how to handle errors and how to structure the codebase for a project like this. We agreed to have set bi-weekly goals for the project to keep us on track and ensure we are making progress towards our milestones. Our goal for next meeting is to have a working prototype debugger that uses the websocket pipe to communicate with a client.
