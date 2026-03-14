import { NestFactory } from "@nestjs/core";
import { AppModule } from "./app.module";
import { NestExpressApplication } from "@nestjs/platform-express";

async function bootstrap() {
  const app = await NestFactory.create<NestExpressApplication>(AppModule);
  app.useBodyParser("json", { limit: "10mb" }); // Increase payload size limit if needed
  await app.listen(3000);
  console.log("NestJS server running on http://localhost:3000");
}
bootstrap();
